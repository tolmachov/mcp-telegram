package authsrv

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

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
	// Enforce the allowlist at the token endpoint too, so a removed user
	// stops minting fresh tokens instead of relying solely on the verifier
	// rejecting them at use.
	if !a.cfg.userAllowed(userID) {
		a.logger.Warn("refresh rejected: user no longer allowed", "user_id", userID)
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "this Telegram account is not allowed")
		return
	}
	exists, err := a.store.Exists(r.Context(), userID)
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

	a.mintTokens(w, mintInput{
		Subject: rc.Subject, Username: rc.Username,
		ClientID: rc.ClientID, Resource: rc.Resource,
		LoginAt: rc.LoginAt,
	})
}

// mintInput carries everything needed to mint an access/refresh token pair.
type mintInput struct {
	Subject, Username  string
	ClientID, Resource string
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
