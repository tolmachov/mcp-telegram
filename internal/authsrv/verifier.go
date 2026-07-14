package authsrv

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// extraIdentityKey is the single TokenInfo.Extra key this package sets. All
// identity material travels as one typed identityExtra value, so a value-type
// drift is a compile error rather than a silently-zero type assertion.
const extraIdentityKey = "mcp-telegram/identity"

// identityExtra is the identity payload the verifier stores in
// TokenInfo.Extra and Identity reads back.
type identityExtra struct {
	ID       tgid.UserID
	Username string
}

// Verifier returns the auth.TokenVerifier for RequireBearerToken. It opens
// the sealed access token, re-checks the allowlist, and exposes the user
// identity via TokenInfo.Extra. TokenInfo.UserID is the decimal Telegram user
// ID, which the streamable transport uses to bind MCP sessions to one user.
//
// Rejections are logged server-side (reason class only, never the token):
// a mass 401 after a botched key rotation or an issuer change must be
// diagnosable from the logs.
func (a *AuthServer) Verifier() auth.TokenVerifier {
	return func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		now := a.now()
		c, err := openBlob(a.sealer, accessBlob, token, now)
		if err != nil {
			a.logger.Warn("access token rejected", "reason", err)
			return nil, fmt.Errorf("%w: not a valid access token", auth.ErrInvalidToken)
		}
		if !now.Before(time.Unix(c.ExpiresAt, 0)) {
			return nil, fmt.Errorf("%w: token expired", auth.ErrInvalidToken)
		}
		userID, err := tgid.Parse(c.Subject)
		if err != nil {
			a.logger.Warn("access token rejected: malformed subject", "reason", err)
			return nil, fmt.Errorf("%w: not a valid access token", auth.ErrInvalidToken)
		}
		// Re-check the allowlist at use time: this is the enforcement point
		// that cuts off already-issued tokens after a user is removed from
		// the allowlist (and the server redeployed).
		if !a.cfg.userAllowed(userID) {
			a.logger.Warn("access token rejected: user no longer allowed", "user_id", userID)
			return nil, fmt.Errorf("%w: user not allowed", auth.ErrInvalidToken)
		}
		return &auth.TokenInfo{
			Expiration: time.Unix(c.ExpiresAt, 0),
			UserID:     c.Subject,
			Extra: map[string]any{
				extraIdentityKey: identityExtra{ID: userID, Username: c.Username},
			},
		}, nil
	}
}

// UserIdentity describes the authenticated user of the current request.
type UserIdentity struct {
	// ID is the numeric Telegram user ID established by the QR login. Use it
	// as the key for per-user resources (sessions, client pools).
	ID tgid.UserID
	// Username is the Telegram @username captured at login (may be empty:
	// usernames are optional and can change; ID is the stable key).
	Username string
}

// Identity returns the authenticated user of the request, or ok=false when
// the request was not authenticated by this package (stdio transport or
// auth disabled). It reads the token frozen at session-connect time from the
// context; for the freshest per-request token use IdentityFromTokenInfo with
// req.GetExtra().TokenInfo.
func Identity(ctx context.Context) (*UserIdentity, bool) {
	return IdentityFromTokenInfo(auth.TokenInfoFromContext(ctx))
}

// IdentityFromTokenInfo decodes the authenticated user from a bearer
// TokenInfo (as this package's Verifier produced), or ok=false when info is
// nil or carries no identity. The MCP SDK attaches the per-request TokenInfo
// to each server request as req.GetExtra().TokenInfo — the freshest source.
func IdentityFromTokenInfo(info *auth.TokenInfo) (*UserIdentity, bool) {
	if info == nil {
		return nil, false
	}
	extra, ok := info.Extra[extraIdentityKey].(identityExtra)
	if !ok || extra.ID <= 0 {
		return nil, false
	}
	return &UserIdentity{ID: extra.ID, Username: extra.Username}, true
}

// NewTokenInfoForTesting fabricates the TokenInfo this package's Verifier
// would produce. It exists so other packages can unit-test handlers that sit
// behind auth.RequireBearerToken (e.g. the per-user client pool) without
// running the OAuth flow.
func NewTokenInfoForTesting(id tgid.UserID, username string, expiry time.Time) *auth.TokenInfo {
	return &auth.TokenInfo{
		UserID:     id.String(),
		Expiration: expiry,
		Extra: map[string]any{
			extraIdentityKey: identityExtra{ID: id, Username: username},
		},
	}
}
