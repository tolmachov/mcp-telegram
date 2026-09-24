package authsrv

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

type oauthErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

const maxFormBody = 64 << 10

func (a *AuthServer) handleToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		a.tokenError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		a.tokenFromCode(w, r, r.PostForm)
	case "refresh_token":
		a.tokenFromRefresh(w, r, r.PostForm)
	default:
		a.tokenError(w, http.StatusBadRequest, "unsupported_grant_type", "supported grant types: authorization_code, refresh_token")
	}
}

func (a *AuthServer) tokenFromCode(w http.ResponseWriter, r *http.Request, form url.Values) {
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
	if resource := form.Get("resource"); resource != "" && normalizeResource(resource) != a.cfg.IssuerURL {
		a.tokenError(w, http.StatusBadRequest, "invalid_target", "unknown resource")
		return
	}
	redeemed, err := a.store.RedeemCode(r.Context(), cc.Family, now.Add(refreshTokenTTL))
	if err != nil {
		a.logger.Error("authorization code state write failed", "err", err)
		a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "authorization state unavailable, retry")
		return
	}
	if !redeemed {
		a.logger.Warn("authorization code replay rejected")
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "authorization code was already used")
		return
	}
	a.mintTokens(w, mintInput{
		Subject: cc.Subject, Username: cc.Username, ClientID: cc.ClientID,
		grant: cc.grantClaims, Generation: 0, LoginAt: now.Unix(),
	})
}

func (a *AuthServer) tokenFromRefresh(w http.ResponseWriter, r *http.Request, form url.Values) {
	now := a.now()
	rc, err := openBlob(a.sealer, refreshBlob, form.Get("refresh_token"), now)
	if err != nil {
		a.logger.Warn("refresh token rejected", "reason", err)
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "invalid refresh token")
		return
	}
	if expired(rc.LoginAt, refreshTokenTTL, now) {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "refresh token expired, log in again")
		return
	}
	if clientID := form.Get("client_id"); clientID != "" && clientID != rc.ClientID {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "client_id mismatch")
		return
	}
	userID := rc.Subject
	if !a.cfg.userAllowed(userID) {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "this Telegram account is not allowed")
		return
	}
	// The two probes are independent store round trips, so they run at once;
	// their answers are still judged revocation first.
	type probe struct {
		ok  bool
		err error
	}
	existsDone := make(chan probe, 1)
	go func() {
		ok, err := a.store.Exists(r.Context(), userID, rc.SessionID)
		existsDone <- probe{ok, err}
	}()
	revoked, err := a.store.Revoked(r.Context(), userID, rc.SessionID)
	exists := <-existsDone
	if err != nil {
		a.logger.Error("revocation check failed on refresh", "user_id", userID, "err", err)
		a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "session check failed, try again")
		return
	}
	if revoked {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "session revoked, log in again")
		return
	}
	if exists.err != nil {
		a.logger.Error("session existence check failed on refresh", "user_id", userID, "err", exists.err)
		a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "session check failed, try again")
		return
	}
	if !exists.ok {
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "telegram session is gone, log in again")
		return
	}

	rotation, err := a.store.RotateGrant(r.Context(), rc.Family, rc.Generation, now)
	if err != nil {
		a.logger.Error("grant rotation failed", "user_id", userID, "err", err)
		a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "authorization state unavailable, retry")
		return
	}
	if rotation != sessionstore.GrantRotated {
		if rotation == sessionstore.GrantReplay {
			if revokeErr := a.store.Revoke(r.Context(), userID, rc.SessionID); revokeErr != nil {
				a.logger.Error("refresh replay session revocation failed", "user_id", userID, "err", revokeErr)
			}
			a.invalidate(userID, rc.SessionID)
			a.logger.Warn("refresh replay revoked grant", "user_id", userID, "session", rc.SessionID)
		}
		a.tokenError(w, http.StatusBadRequest, "invalid_grant", "refresh token replayed or grant expired; log in again")
		return
	}

	a.mintTokens(w, mintInput{
		Subject: rc.Subject, Username: rc.Username, ClientID: rc.ClientID,
		grant: rc.grantClaims, Generation: rc.Generation + 1, LoginAt: rc.LoginAt,
	})
}

type mintInput struct {
	Subject            tgid.UserID
	Username, ClientID string
	grant              grantClaims
	Generation         int64
	LoginAt            int64
}

// mintTokens issues an access and refresh token pair. sealBlob refuses claims
// that would not open again, which answers 500.
func (a *AuthServer) mintTokens(w http.ResponseWriter, in mintInput) {
	now := a.now()
	expiresAt := now.Add(accessTokenTTL)
	accessToken, err := sealBlob(a.sealer, accessBlob, accessClaims{
		Subject: in.Subject, Username: in.Username, ClientID: in.ClientID,
		grantClaims: in.grant, IssuedAt: now.Unix(), ExpiresAt: expiresAt.Unix(),
	})
	if err != nil {
		a.logger.Error("sealing access token failed", "err", err)
		a.tokenError(w, http.StatusInternalServerError, "server_error", "internal error")
		return
	}
	refreshToken, err := sealBlob(a.sealer, refreshBlob, refreshClaims{
		Subject: in.Subject, Username: in.Username, ClientID: in.ClientID,
		grantClaims: in.grant, Generation: in.Generation, IssuedAt: now.Unix(), LoginAt: in.LoginAt,
	})
	if err != nil {
		a.logger.Error("sealing refresh token failed", "err", err)
		a.tokenError(w, http.StatusInternalServerError, "server_error", "internal error")
		return
	}
	a.writeJSON(w, http.StatusOK, &tokenResponse{AccessToken: accessToken, TokenType: "Bearer", ExpiresIn: int64(expiresAt.Sub(now).Seconds()), RefreshToken: refreshToken})
}

// normalizeResource strips trailing slashes from a resource a client
// presents, so "https://host/" matches the issuer. Sealed claims always carry
// the issuer verbatim and are compared exactly.
func normalizeResource(resource string) string { return strings.TrimRight(resource, "/") }

func verifyPKCE(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

func (a *AuthServer) tokenError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	a.writeJSON(w, status, &oauthErrorResponse{Error: code, ErrorDescription: description})
}
