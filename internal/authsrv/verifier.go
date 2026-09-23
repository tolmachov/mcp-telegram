package authsrv

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// extraIdentityKey is the single TokenInfo.Extra key this package sets. The
// identity travels as one UserIdentity value that only this package writes
// and reads, so IdentityFromTokenInfo asserts exactly that type; any other
// value under the key reads as no identity.
const extraIdentityKey = "mcp-telegram/identity"

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
		if !c.valid(a.cfg.IssuerURL) {
			a.logger.Warn("access token rejected: malformed grant identity or foreign resource", "user_id", userID)
			return nil, fmt.Errorf("%w: not a valid access token", auth.ErrInvalidToken)
		}
		u := UserIdentity{ID: userID, Username: c.Username, SessionID: c.SessionID, SessionKey: c.SessionKey}
		return u.TokenInfo(time.Unix(c.ExpiresAt, 0)), nil
	}
}

// UserIdentity describes the authenticated user of the current request.
type UserIdentity struct {
	// ID is the numeric Telegram user ID established by the QR login.
	ID tgid.UserID
	// Username is the Telegram @username captured at login (may be empty:
	// usernames are optional and can change; ID is the stable key).
	Username string
	// SessionID is this authorization's session-object suffix. Together with ID it keys the client pool,
	// so multiple independent authorizations of one account each get their own
	// assembly.
	SessionID string
	// SessionKey is this authorization's per-session encryption key from the
	// token. It is passed to the session store to decrypt this session's blob
	// Treat as secret: never log it.
	SessionKey []byte
}

// TokenInfo encodes u as the bearer TokenInfo that IdentityFromTokenInfo
// reads back. TokenInfo.UserID is the decimal Telegram user ID.
func (u UserIdentity) TokenInfo(expires time.Time) *auth.TokenInfo {
	return &auth.TokenInfo{
		Expiration: expires,
		UserID:     u.ID.String(),
		Extra:      map[string]any{extraIdentityKey: u},
	}
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
	u, ok := info.Extra[extraIdentityKey].(UserIdentity)
	if !ok || u.ID <= 0 {
		return nil, false
	}
	return &u, true
}
