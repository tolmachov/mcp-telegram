package authsrv

import "net/http"

// handleRevoke implements RFC 7009 as a spec-compliant no-op. Sealed tokens
// cannot be individually invalidated (stateless design) and there is no
// upstream IdP grant to kill, so the endpoint acknowledges every request
// with 200 (§2.2 requires that even for invalid tokens). Real revocation is
// operational: delete the user's Telegram session (refreshes then fail) or
// remove them from the allowlist (existing tokens stop verifying).
func (a *AuthServer) handleRevoke(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		a.tokenError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	a.logger.Debug("revocation acknowledged (stateless no-op)")
	w.WriteHeader(http.StatusOK)
}
