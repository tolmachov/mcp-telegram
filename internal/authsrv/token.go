package authsrv

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// errLegacyAlreadyUpgraded reports that a concurrent (or prior) refresh already
// migrated this account's legacy session, so the presented legacy token is now
// stale and its holder must log in again.
var errLegacyAlreadyUpgraded = errors.New("legacy session already upgraded")

// tokenResponse is the RFC 6749 §5.1 success payload.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// oauthErrorResponse is the RFC 6749 §5.2 error payload.
type oauthErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// maxFormBody bounds the form bodies of the auth endpoints. Sealed refresh
// tokens are well under 1 KB, so 64 KiB is generous.
const maxFormBody = 64 << 10

// handleToken implements the token endpoint for the authorization_code and
// refresh_token grants. All clients are public: no client authentication,
// PKCE is the proof of possession. The parsed form is passed down explicitly:
// the body size is bounded exactly once, here.
func (a *AuthServer) handleToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		a.tokenError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	form := r.PostForm
	switch form.Get("grant_type") {
	case "authorization_code":
		a.tokenFromCode(w, form)
	case "refresh_token":
		a.tokenFromRefresh(w, r, form)
	default:
		a.tokenError(w, http.StatusBadRequest, "unsupported_grant_type",
			"supported grant types: authorization_code, refresh_token")
	}
}

// tokenFromCode redeems a sealed authorization code.
func (a *AuthServer) tokenFromCode(w http.ResponseWriter, form url.Values) {
	now := a.now()
	cc, err := openBlob(a.sealer, codeBlob, form.Get("code"), now)
	if err != nil {
		a.logger.Warn("authorization code rejected", "reason", err)
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired authorization code")
		return
	}
	if form.Get("client_id") != cc.ClientID {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	}
	if ru := form.Get("redirect_uri"); ru != "" && ru != cc.RedirectURI {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
		return
	}
	if !verifyPKCE(form.Get("code_verifier"), cc.CodeChallenge) {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	if res := form.Get("resource"); res != "" && strings.TrimRight(res, "/") != a.cfg.IssuerURL {
		a.tokenError(w, http.StatusBadRequest, "invalid_target", "unknown resource")
		return
	}

	a.mintTokens(w, mintInput{
		Subject: cc.Subject, Username: cc.Username,
		ClientID: cc.ClientID, Resource: cc.Resource,
		SessionID: cc.SessionID, SessionKey: cc.SessionKey,
		LoginAt: now.Unix(),
	})
}

// tokenFromRefresh redeems a sealed refresh token for a fresh access/refresh
// pair. The grant is re-gated here: the subject must still be on the
// allowlist AND still have a stored Telegram session — deleting a user's
// session (logout, AUTH_KEY_UNREGISTERED, operator cleanup) forces a full
// re-login on the next refresh. The original login time is preserved so the
// refresh-token TTL is absolute.
func (a *AuthServer) tokenFromRefresh(w http.ResponseWriter, r *http.Request, form url.Values) {
	rc, err := openBlob(a.sealer, refreshBlob, form.Get("refresh_token"), a.now())
	if err != nil {
		a.logger.Warn("refresh token rejected", "reason", err)
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "invalid refresh token")
		return
	}
	if expired(rc.LoginAt, a.cfg.refreshTokenTTL(), a.now()) {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "refresh token expired, log in again")
		return
	}
	if cid := form.Get("client_id"); cid != "" && cid != rc.ClientID {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	}
	userID, err := tgid.Parse(rc.Subject)
	if err != nil {
		a.logger.Warn("refresh rejected: malformed subject", "subject", rc.Subject, "err", err)
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "invalid refresh token")
		return
	}
	// A non-empty sid from the token becomes a storage object-name/path suffix
	// below; reject a malformed one here (a forged/corrupt token) so it can never
	// reach the storage layer.
	if rc.SessionID != "" && !validSessionID(rc.SessionID) {
		a.logger.Warn("refresh rejected: malformed session id", "user_id", userID)
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "invalid refresh token")
		return
	}
	// Enforce the allowlist at the token endpoint too, so a removed user
	// stops minting fresh tokens instead of relying solely on the verifier
	// rejecting them at use.
	if !a.cfg.userAllowed(userID) {
		a.logger.Warn("refresh rejected: user no longer allowed", "user_id", userID)
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "this Telegram account is not allowed")
		return
	}
	exists, err := a.store.Exists(r.Context(), userID, rc.SessionID)
	if err != nil {
		// Backend failure, not a dead grant: 503 keeps the client retrying
		// instead of discarding a refresh token that may still be good.
		a.logger.Error("session existence check failed on refresh", "user_id", userID, "err", err)
		a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "session check failed, try again")
		return
	}
	if !exists {
		a.logger.Warn("refresh rejected: telegram session gone", "user_id", userID)
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "telegram session is gone, log in again")
		return
	}

	// Migrate a legacy (master-only) session to an independent split-key session
	// on this refresh, so protection completes within one access-token lifetime
	// without forcing a fresh QR login.
	sid, sessionKey := rc.SessionID, rc.SessionKey
	if sid == "" {
		upgradedSID, upgradedKey, upErr := a.upgradeLegacySession(r.Context(), userID)
		switch {
		case errors.Is(upErr, errLegacyAlreadyUpgraded):
			// The legacy object is gone (a concurrent refresh won the upgrade, or
			// it was revoked/swept); this legacy token is stale.
			a.logger.Info("legacy refresh rejected: session already migrated or deleted", "user_id", userID)
			a.tokenError(w, http.StatusBadRequest, "invalid_grant", "session migrated, log in again")
			return
		case upErr != nil:
			// Transient failure: keep the (still-present) legacy session and
			// re-issue legacy tokens rather than break the refresh.
			a.logger.Warn("legacy session upgrade skipped; keeping master-only session", "user_id", userID, "err", upErr)
		default:
			sid, sessionKey = upgradedSID, upgradedKey
			a.logger.Info("upgraded legacy session to split-key on refresh", "user_id", userID)
		}
	}

	a.mintTokens(w, mintInput{
		Subject: rc.Subject, Username: rc.Username,
		ClientID: rc.ClientID, Resource: rc.Resource,
		SessionID: sid, SessionKey: sessionKey,
		LoginAt: rc.LoginAt,
	})
}

// upgradeLegacySession MOVES a user's legacy master-only session into a fresh
// independent split-key session: it copies the blob to a new (sid, key) object,
// then deletes the legacy object and tears down any warm assembly still serving
// it. The move (rather than a copy) is deliberate and load-bearing:
//
//   - It invalidates the old stateless legacy refresh token: its next use fails
//     the Exists(userID, "") gate, so a replayed or un-rotated legacy token
//     cannot keep minting tokens or spawn a fresh copy per refresh.
//   - It prevents two live gotd clients from running on the same MTProto auth
//     key (the just-created copy plus the still-warm legacy assembly), which
//     Telegram punishes with AUTH_KEY_DUPLICATED, force-logging out the account.
//
// Trade-off: the FIRST legacy client to refresh wins the upgrade; any other
// legacy client of the same account then fails its next refresh with
// invalid_grant and re-runs the QR login. That is strictly better than the
// silent auth-key duplication the copy-based version risked.
//
// Concurrency and atomicity: the whole load→write→delete sequence runs under
// upgradeMu, so two simultaneous legacy refreshes cannot both produce a copy of
// the same auth key — the loser observes the legacy object already gone
// (session.ErrNotFound) and is told to log in again. If deleting the legacy
// object fails, the just-written object is rolled back so the account is never
// left with two live objects sharing one auth key.
func (a *AuthServer) upgradeLegacySession(ctx context.Context, userID tgid.UserID) (string, []byte, error) {
	unlock := a.lockUpgrade(userID)
	defer unlock()

	data, err := a.store.Session(userID, "", nil).LoadSession(ctx)
	if errors.Is(err, session.ErrNotFound) {
		// Under the per-account lock, a missing legacy object means it was
		// already migrated by a prior/concurrent refresh (or deleted by
		// revoke/logout/sweep) — either way this legacy token is stale and its
		// holder must log in again.
		return "", nil, errLegacyAlreadyUpgraded
	}
	if err != nil {
		return "", nil, fmt.Errorf("reading legacy session: %w", err)
	}
	sid, key, err := newSessionCreds()
	if err != nil {
		return "", nil, err
	}
	if err := a.store.Session(userID, sid, key).StoreSession(ctx, data); err != nil {
		return "", nil, fmt.Errorf("writing upgraded session: %w", err)
	}
	// Stop any warm client on the legacy session before removing the object, so
	// it cannot re-store (resurrect) the blob after the delete.
	a.invalidateSession(userID, "")
	if err := a.store.Delete(ctx, userID, ""); err != nil {
		// Roll back the new object: leaving BOTH live would let the legacy token
		// upgrade again into a second holder of the same auth key. Best-effort —
		// a surviving orphan is reclaimed by the sweeper.
		if delErr := a.store.Delete(ctx, userID, sid); delErr != nil {
			a.logger.Error("legacy upgrade rollback failed; orphan will be swept", "user_id", userID, "session", sid, "err", delErr)
		}
		return "", nil, fmt.Errorf("deleting legacy object: %w", err)
	}
	return sid, key, nil
}

// mintInput carries everything needed to mint an access/refresh token pair.
type mintInput struct {
	Subject, Username  string
	ClientID, Resource string
	// SessionID and SessionKey identify and decrypt the caller's own session
	// object; they are sealed into both minted tokens (empty for legacy
	// master-only sessions).
	SessionID  string
	SessionKey []byte
	// LoginAt anchors the refresh-token TTL: it must be the ORIGINAL login
	// time (now for the code grant, the previous token's LoginAt for the
	// refresh grant). Passing "now" on refresh would make refresh tokens
	// infinitely renewable; mintTokens rejects values in the future.
	LoginAt int64
}

// mintTokens seals and writes the token response.
func (a *AuthServer) mintTokens(w http.ResponseWriter, in mintInput) {
	now := a.now()
	if in.LoginAt <= 0 || in.LoginAt > now.Unix() {
		a.logger.Error("mint rejected: implausible login time", "subject", in.Subject, "login_at", in.LoginAt)
		a.tokenError(w, http.StatusInternalServerError, "server_error", "internal error")
		return
	}
	exp := now.Add(accessTokenTTL)

	accessToken, err := sealBlob(a.sealer, accessBlob, accessClaims{
		Subject: in.Subject, Username: in.Username,
		ClientID: in.ClientID, Resource: in.Resource,
		SessionID: in.SessionID, SessionKey: in.SessionKey,
		IssuedAt: now.Unix(), ExpiresAt: exp.Unix(),
	})
	if err != nil {
		a.logger.Error("sealing access token failed", "err", err)
		a.tokenError(w, http.StatusInternalServerError, "server_error", "internal error")
		return
	}
	refreshToken, err := sealBlob(a.sealer, refreshBlob, refreshClaims{
		Subject: in.Subject, Username: in.Username,
		ClientID: in.ClientID, Resource: in.Resource,
		SessionID: in.SessionID, SessionKey: in.SessionKey,
		IssuedAt: now.Unix(), LoginAt: in.LoginAt,
	})
	if err != nil {
		a.logger.Error("sealing refresh token failed", "err", err)
		a.tokenError(w, http.StatusInternalServerError, "server_error", "internal error")
		return
	}

	a.writeJSON(w, http.StatusOK, &tokenResponse{
		AccessToken:  accessToken,
		TokenType:    "Bearer",
		ExpiresIn:    int64(exp.Sub(now).Seconds()),
		RefreshToken: refreshToken,
	})
}

// verifyPKCE checks S256(verifier) == challenge in constant time.
func verifyPKCE(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// tokenError writes an RFC 6749 §5.2 error response.
func (a *AuthServer) tokenError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	a.writeJSON(w, status, &oauthErrorResponse{Error: code, ErrorDescription: description})
}
