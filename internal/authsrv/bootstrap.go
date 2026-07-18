package authsrv

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"rsc.io/qr"
)

// Pending-login registry limits. The TTL matches Telegram's own QR login
// window order of magnitude; the cap bounds how many MTProto clients a
// drive-by /authorize flood can spawn.
const (
	pendingLoginTTL  = 5 * time.Minute
	maxPendingLogins = 16
	// maxPendingLoginsPerIP caps how many of the global slots a single client
	// IP may hold at once. Without it one IP — whose per-IP rate-limit burst
	// (rateLimitBurst) exceeds the global cap — could fill every slot with
	// abandoned flows and lock all other users out until the entries expire.
	// A per-IP fraction of the global cap keeps slots available for others
	// while still allowing a user to retry a few times.
	maxPendingLoginsPerIP = 4
	// finalizeTimeout bounds the durable session write after a successful
	// scan. It runs on a detached context (not the poll request's), so this
	// is the only thing stopping a wedged storage backend from blocking the
	// poll handler indefinitely.
	finalizeTimeout = 15 * time.Second
)

// errTooManyLogins is returned by addPending when the registry is full.
var errTooManyLogins = fmt.Errorf("too many concurrent login attempts")

// pendingLogin is one /authorize request waiting for its Telegram QR login
// to complete. It pairs the OAuth request (kept as the sealed state blob, so
// finalizeLogin re-validates integrity and the 10-minute state TTL through
// openBlob) with the live flow.
type pendingLogin struct {
	id      string
	request string // sealed stateClaims blob
	flow    LoginFlow
	created time.Time
	ip      string // client IP that started the flow, for the per-IP cap

	mu       sync.Mutex
	lastURL  string
	qrRev    int
	consumed bool
}

// qrState returns the current tg://login URL and its revision counter. The
// revision increments whenever Telegram rotates the token, which the poll
// response exposes so the page knows to re-fetch the QR image.
func (p *pendingLogin) qrState() (u string, rev int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if u, ok := p.flow.TokenURL(); ok && u != p.lastURL {
		p.lastURL = u
		p.qrRev++
	}
	return p.lastURL, p.qrRev, p.lastURL != ""
}

// tryConsume marks the completed flow as claimed by exactly one poll request.
func (p *pendingLogin) tryConsume() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.consumed {
		return false
	}
	p.consumed = true
	return true
}

// newLoginID returns a 128-bit random hex login identifier.
func newLoginID() (string, error) {
	return randomHex(16)
}

// addPending registers a login flow under a fresh random id. It fails with
// errTooManyLogins when the registry is at global capacity or the requesting
// IP already holds its per-IP share (both checked after sweeping expired
// entries); the caller must then abort the flow itself.
func (a *AuthServer) addPending(request string, flow LoginFlow, ip string) (*pendingLogin, error) {
	id, err := newLoginID()
	if err != nil {
		return nil, err
	}
	p := &pendingLogin{id: id, request: request, flow: flow, created: a.now(), ip: ip}

	a.pendingMu.Lock()
	a.sweepExpiredLocked(a.now())
	if len(a.pending) >= maxPendingLogins {
		a.pendingMu.Unlock()
		return nil, errTooManyLogins
	}
	// Per-IP cap: count this IP's live entries. The registry holds at most
	// maxPendingLogins (16) entries, so the scan is trivially cheap and needs
	// no separate counter to keep in sync across every removal path.
	if ip != "" {
		perIP := 0
		for _, e := range a.pending {
			if e.ip == ip {
				perIP++
			}
		}
		if perIP >= maxPendingLoginsPerIP {
			a.pendingMu.Unlock()
			return nil, errTooManyLogins
		}
	}
	a.pending[id] = p
	a.pendingMu.Unlock()
	return p, nil
}

// lookupPending returns the live pending login with the given id. Entries
// past their TTL are treated as absent and aborted on the spot, so expiry
// does not depend on janitor timing.
func (a *AuthServer) lookupPending(id string) (*pendingLogin, bool) {
	a.pendingMu.Lock()
	p, ok := a.pending[id]
	if ok && a.now().Sub(p.created) > pendingLoginTTL {
		delete(a.pending, id)
		a.pendingMu.Unlock()
		p.flow.Abort()
		return nil, false
	}
	a.pendingMu.Unlock()
	return p, ok
}

// removePending drops the entry and aborts its flow (Abort is idempotent, so
// this is safe after a completed login too).
func (a *AuthServer) removePending(id string) {
	a.pendingMu.Lock()
	p, ok := a.pending[id]
	delete(a.pending, id)
	a.pendingMu.Unlock()
	if ok {
		p.flow.Abort()
	}
}

// sweepExpiredLocked evicts entries past the TTL and aborts their flows. It
// runs with pendingMu held; calling Abort under the lock is safe because
// Abort is non-blocking per the LoginFlow contract (it only cancels a
// context). The two-pass structure (collect, then abort) keeps the abort
// calls out of the map-iteration loop, not out of the lock.
func (a *AuthServer) sweepExpiredLocked(now time.Time) {
	var stale []*pendingLogin
	for id, p := range a.pending {
		if now.Sub(p.created) > pendingLoginTTL {
			delete(a.pending, id)
			stale = append(stale, p)
		}
	}
	for _, p := range stale {
		p.flow.Abort()
	}
}

// sweepExpiredPending is the janitor entry point (and a test seam).
func (a *AuthServer) sweepExpiredPending() {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	a.sweepExpiredLocked(a.now())
}

// janitor periodically sweeps expired pending logins until ctx is canceled.
func (a *AuthServer) janitor(ctx context.Context) {
	defer close(a.janitorDone)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.sweepExpiredPending()
		}
	}
}

// handleLoginQR serves the current tg://login URL of a pending login as a
// PNG QR code. 404 for unknown/expired ids and before the first token export
// (the page retries when the poll response bumps qr_rev).
func (a *AuthServer) handleLoginQR(w http.ResponseWriter, r *http.Request) {
	p, ok := a.lookupPending(r.URL.Query().Get("login"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	u, _, ok := p.qrState()
	if !ok {
		http.NotFound(w, r)
		return
	}
	code, err := qr.Encode(u, qr.M)
	if err != nil {
		a.logger.Error("encoding QR code failed", "err", err)
		http.Error(w, "QR encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(code.PNG()); err != nil {
		a.logger.Debug("writing QR image failed", "err", err)
	}
}

// pollResponse is the JSON payload of /login/poll.
type pollResponse struct {
	// Status is one of waiting|password|done|failed|expired.
	Status string `json:"status"`
	// Redirect is the client redirect URI carrying the authorization code;
	// set only with status done.
	Redirect string `json:"redirect,omitempty"`
	// QRRev increments whenever the QR token rotates; the page re-fetches
	// the image when it changes.
	QRRev int `json:"qr_rev,omitempty"`
	// Message is a human-readable detail for password/failed/expired.
	Message string `json:"message,omitempty"`
}

// handleLoginPoll reports the state of a pending login and, exactly once per
// successful login, finalizes it: allowlist check, session persistence, and
// authorization-code minting.
func (a *AuthServer) handleLoginPoll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p, ok := a.lookupPending(r.URL.Query().Get("login"))
	if !ok {
		a.writeJSON(w, http.StatusOK, &pollResponse{
			Status: "expired", Message: "The login expired. Start over from your MCP client.",
		})
		return
	}
	switch p.flow.State() {
	case LoginWaiting:
		_, rev, _ := p.qrState()
		a.writeJSON(w, http.StatusOK, &pollResponse{Status: "waiting", QRRev: rev})
	case LoginPasswordNeeded:
		resp := &pollResponse{Status: "password"}
		if p.flow.Err() != nil {
			resp.Message = "Wrong password, try again."
		}
		a.writeJSON(w, http.StatusOK, resp)
	case LoginFailed:
		err := p.flow.Err()
		a.logger.Warn("telegram login failed", "err", err)
		a.removePending(p.id)
		a.writeJSON(w, http.StatusOK, &pollResponse{
			Status: "failed", Message: "Telegram login failed. Start over from your MCP client.",
		})
	case LoginDone:
		if !p.tryConsume() {
			// Another poll is finalizing; report waiting until it finishes.
			a.writeJSON(w, http.StatusOK, &pollResponse{Status: "waiting"})
			return
		}
		// Done() closes immediately after State becomes LoginDone (the flow
		// closes it after full teardown), so this receive blocks at most
		// momentarily.
		<-p.flow.Done()
		// Persist on a server-lifetime context, not the poll request's: the
		// QR scan already succeeded on Telegram's side, so a client that
		// cancels this poll (tab closed at the wrong instant) must not abort
		// the durable session write and discard a login the user just made.
		// The response still goes to this request's ResponseWriter; only the
		// storage write is detached, bounded by a short timeout.
		storeCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), finalizeTimeout)
		defer cancel()
		a.finalizeLogin(storeCtx, w, p)
	default:
		a.logger.Error("pending login in impossible state", "state", p.flow.State())
		a.removePending(p.id)
		a.writeJSON(w, http.StatusOK, &pollResponse{
			Status: "failed", Message: "Internal error. Start over from your MCP client.",
		})
	}
}

// finalizeLogin turns a completed QR login into an authorization code:
// allowlist gate first (a disallowed account's session is never persisted),
// then session storage, then the sealed code.
func (a *AuthServer) finalizeLogin(ctx context.Context, w http.ResponseWriter, p *pendingLogin) {
	fail := func(msg string) {
		a.removePending(p.id)
		a.writeJSON(w, http.StatusOK, &pollResponse{Status: "failed", Message: msg})
	}

	sc, err := openBlob(a.sealer, stateBlob, p.request, a.now())
	if err != nil {
		// Defense in depth: the registry TTL (5m) is stricter than the state
		// TTL (10m), so this fires only on clock jumps or memory corruption.
		a.logger.Warn("pending login carried an invalid authorization request", "reason", err)
		fail("The authorization request expired. Start over from your MCP client.")
		return
	}

	user, ok := p.flow.User()
	if !ok {
		a.logger.Error("login flow done without user identity")
		fail("Telegram login failed. Start over from your MCP client.")
		return
	}
	if !a.cfg.userAllowed(user.ID) {
		a.logger.Warn("login rejected: telegram account not on allowlist", "user_id", user.ID, "username", user.Username)
		fail("This Telegram account is not allowed.")
		return
	}
	data, ok := p.flow.SessionData()
	if !ok || len(data) == 0 {
		a.logger.Error("login flow done without session data", "user_id", user.ID)
		fail("Telegram login failed. Start over from your MCP client.")
		return
	}
	// Each login is an independent session: a fresh random sid (its own bucket
	// object) and a fresh random key (folded into the session encryption, carried
	// only in the tokens below). Concurrent logins for one account therefore do
	// not overwrite each other.
	sid, sessionKey, err := newSessionCreds()
	if err != nil {
		a.logger.Error("generating session credentials failed", "user_id", user.ID, "err", err)
		fail("Internal error. Start over from your MCP client.")
		return
	}
	if err := a.store.Session(user.ID, sid, sessionKey).StoreSession(ctx, data); err != nil {
		a.logger.Error("storing telegram session failed", "user_id", user.ID, "err", err)
		fail("Storing the Telegram session failed. Start over from your MCP client.")
		return
	}

	now := a.now()
	code, err := sealBlob(a.sealer, codeBlob, codeClaims{
		Subject:       user.ID.String(),
		Username:      user.Username,
		ClientID:      sc.ClientID,
		RedirectURI:   sc.RedirectURI,
		CodeChallenge: sc.CodeChallenge,
		Resource:      sc.Resource,
		SessionID:     sid,
		SessionKey:    sessionKey,
		IssuedAt:      now.Unix(),
	})
	if err != nil {
		a.logger.Error("sealing authorization code failed", "err", err)
		fail("Internal error. Start over from your MCP client.")
		return
	}
	redirect, err := buildCodeRedirect(sc.RedirectURI, code, sc.State)
	if err != nil {
		a.logger.Error("building redirect failed", "err", err)
		fail("Internal error. Start over from your MCP client.")
		return
	}

	a.logger.Info("telegram login completed", "user_id", user.ID, "username", user.Username)
	a.removePending(p.id)
	a.writeJSON(w, http.StatusOK, &pollResponse{Status: "done", Redirect: redirect})
}

// buildCodeRedirect appends code and state to the (pre-validated) client
// redirect URI.
func buildCodeRedirect(redirectURI, code, state string) (string, error) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", fmt.Errorf("parsing redirect URI: %w", err)
	}
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// handleLoginPassword feeds the 2FA cloud password into a pending flow. A
// malformed form is a 400 and an unknown/expired login id is a 404, but a
// valid submission always gets 204 regardless of whether the password was
// right — correctness surfaces only in the next poll, keeping this endpoint
// free of oracle behavior beyond what the login flow itself reveals.
func (a *AuthServer) handleLoginPassword(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form body", http.StatusBadRequest)
		return
	}
	p, ok := a.lookupPending(r.PostForm.Get("login"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !p.flow.SubmitPassword(r.PostForm.Get("password")) {
		a.logger.Debug("password submission ignored: flow not awaiting password")
	}
	w.WriteHeader(http.StatusNoContent)
}
