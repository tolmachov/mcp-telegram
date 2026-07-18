package authsrv

import (
	"net/http"

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
//   - A legacy token (empty sid) tombstones the shared legacy per-user session,
//     revoking ALL of that account's not-yet-upgraded legacy clients —
//     acceptable while the legacy fallback exists at all.
//   - This does NOT terminate the Telegram-side device authorization (it stays
//     listed in Settings → Devices until Telegram expires it); a full
//     auth.LogOut on revoke is a possible follow-up.
//
// Possession of a decryptable token is sufficient authorization to revoke it
// (§2.1 — all our clients are public); expiry does not block revocation. Every
// response is 200 (§2.2 requires that even for invalid tokens, so there is no
// validity oracle); the reason class is logged server-side, never the token.
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
	sub, sid, clientID, opened := a.openRevocationTarget(token, r.PostForm.Get("token_type_hint"))
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
	// A non-empty sid becomes a storage object-name/path suffix below; a
	// malformed one means a forged/corrupt token — acknowledge without touching
	// storage.
	if sid != "" && !validSessionID(sid) {
		a.logger.Warn("revocation ignored: malformed session id", "user_id", userID)
		ok()
		return
	}
	// Durably mark the session revoked (a tombstone) and delete its blob. The
	// tombstone — not the delete — is what makes revocation reliable: a warm
	// gotd client may re-store (resurrect) the blob, but the refresh gate checks
	// Revoked, so the grant stays dead regardless. The already-issued access
	// token remains valid until it expires (<= accessTokenTTL); revocation stops
	// renewal, matching the short-lived-access / revocable-refresh model.
	if err := a.store.Revoke(r.Context(), userID, sid); err != nil {
		// The client cannot retry meaningfully and must not learn store
		// internals; log for the operator.
		a.logger.Error("revocation could not be recorded", "user_id", userID, "session", sid, "err", err)
		ok()
		return
	}
	// Best-effort: drop the pooled assembly so the connection is freed promptly.
	// Not required for correctness (the tombstone is) and may be a no-op.
	a.invalidateSession(userID, sid)
	a.logger.Info("token revoked", "user_id", userID, "session", sid)
	ok()
}

// openRevocationTarget opens a token presented for revocation and returns its
// subject, session id, and client id. Per RFC 7009 §2.1 token_type_hint only
// orders the attempts: the hinted kind is tried first, then the other kind.
func (a *AuthServer) openRevocationTarget(token, hint string) (sub, sid, clientID string, ok bool) {
	tryRefresh := func() bool {
		rc, err := openBlob(a.sealer, refreshBlob, token, a.now())
		if err != nil {
			return false
		}
		sub, sid, clientID = rc.Subject, rc.SessionID, rc.ClientID
		return true
	}
	tryAccess := func() bool {
		ac, err := openBlob(a.sealer, accessBlob, token, a.now())
		if err != nil {
			return false
		}
		sub, sid, clientID = ac.Subject, ac.SessionID, ac.ClientID
		return true
	}
	if hint == "access_token" {
		ok = tryAccess() || tryRefresh()
	} else {
		ok = tryRefresh() || tryAccess()
	}
	return sub, sid, clientID, ok
}
