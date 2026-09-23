package authsrv

import (
	"net/http"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// handleRevoke implements RFC 7009. Revoking a token writes a durable
// revocation tombstone for its (userID, sid) session and deletes the blob. The
// tombstone — not the delete — is the reliable part: the refresh grant checks
// Revoked, so even if a warm gotd client re-stores (resurrects) the blob, the
// grant stays dead. Limits, stated plainly:
//
//   - Already-issued access tokens are stateless and keep verifying until they
//     expire (<= accessTokenTTL); revocation stops renewal (the refresh grant),
//     not the current short-lived access token. The pooled assembly is dropped
//     best-effort to free the connection.
//   - This does NOT terminate the Telegram-side device authorization (it stays
//     listed in Settings → Devices until Telegram expires it); a full
//     auth.LogOut on revoke is a possible follow-up.
//
// Possession of a decryptable token is sufficient authorization to revoke it
// (§2.1 — all our clients are public); expiry does not block revocation. A
// recognized token whose tombstone cannot be written answers 503
// temporarily_unavailable (§2.2.1) so the client retries instead of assuming
// the grant is dead; every other outcome — including an unrecognized/invalid
// token — is 200, so there is no validity oracle. The reason class is logged
// server-side, never the token.
func (a *AuthServer) handleRevoke(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		a.tokenError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	ok := func() {
		w.WriteHeader(http.StatusOK)
	}

	token := r.PostForm.Get("token")
	if token == "" {
		ok()
		return
	}
	sub, sid, family, clientID, opened := a.openRevocationTarget(token, r.PostForm.Get("token_type_hint"))
	if !opened {
		a.logger.Debug("revocation of an unrecognized token acknowledged")
		ok()
		return
	}
	if cid := r.PostForm.Get("client_id"); cid != "" && cid != clientID {
		a.logger.Warn("revocation ignored: client_id mismatch")
		ok()
		return
	}
	userID, err := tgid.Parse(sub)
	if err != nil {
		a.logger.Warn("revocation ignored: malformed subject", "err", err)
		ok()
		return
	}
	// Durably mark the session revoked (a tombstone) and delete its blob. The
	// tombstone — not the delete — is what makes revocation reliable: a warm
	// gotd client may re-store (resurrect) the blob, but the refresh gate checks
	// Revoked, so the grant stays dead regardless. The already-issued access
	// token remains valid until it expires (<= accessTokenTTL); revocation stops
	// renewal, matching the short-lived-access / revocable-refresh model.
	if err := sessionstore.RevokeGrant(r.Context(), a.store, family); err != nil {
		a.logger.Error("grant revocation could not be recorded", "user_id", userID, "err", err)
		w.Header().Set("Retry-After", "60")
		a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "revocation could not be recorded, retry")
		return
	}
	if err := a.store.Revoke(r.Context(), userID, sid); err != nil {
		// The tombstone is the reliable part of revocation; if it cannot be
		// written the grant is still live, so we must NOT answer 200 (which the
		// client reads as "the token is dead"). Signal a retryable failure per
		// §2.2.1 and log the class for the operator, never the token.
		a.logger.Error("revocation could not be recorded", "user_id", userID, "session", sid, "err", err)
		// RFC 7009 §2.2.1: the 503 may carry Retry-After to pace client retries.
		w.Header().Set("Retry-After", "60")
		a.tokenError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "revocation could not be recorded, retry")
		return
	}
	// Best-effort: drop the pooled assembly so the connection is freed promptly.
	// Not required for correctness (the tombstone is) and may be a no-op.
	a.invalidate(userID, sid)
	a.logger.Info("token revoked", "user_id", userID, "session", sid)
	ok()
}

// openRevocationTarget opens a token presented for revocation and returns its
// subject, session id, family and client id. Per RFC 7009 §2.1 token_type_hint
// only orders the attempts: the hinted kind is tried first, then the other
// kind. A token with a malformed grant identity (forged or corrupt) counts as
// unrecognized, so revocation never touches storage with it.
func (a *AuthServer) openRevocationTarget(token, hint string) (sub, sid, family, clientID string, ok bool) {
	tryRefresh := func() bool {
		rc, err := openBlob(a.sealer, refreshBlob, token, a.now())
		if err != nil || !rc.valid(a.cfg.IssuerURL) {
			return false
		}
		sub, sid, family, clientID = rc.Subject, rc.SessionID, rc.Family, rc.ClientID
		return true
	}
	tryAccess := func() bool {
		ac, err := openBlob(a.sealer, accessBlob, token, a.now())
		if err != nil || !ac.valid(a.cfg.IssuerURL) {
			return false
		}
		sub, sid, family, clientID = ac.Subject, ac.SessionID, ac.Family, ac.ClientID
		return true
	}
	if hint == "access_token" {
		ok = tryAccess() || tryRefresh()
	} else {
		ok = tryRefresh() || tryAccess()
	}
	return sub, sid, family, clientID, ok
}
