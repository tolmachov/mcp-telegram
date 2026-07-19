package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

const (
	// userPoolIdleTTL is how long a user's assembly (MCP servers + the live
	// MTProto client) survives without requests before eviction. Sliding:
	// every request resets it. Evicted users are rebuilt transparently on
	// their next request from the stored session.
	userPoolIdleTTL = 15 * time.Minute
	// userPoolMaxUsers caps concurrent assemblies (one per authorization, so an
	// account logged in from several clients consumes several slots). Each holds
	// a live MTProto TCP connection plus its goroutines, which is heavier than a
	// set of stateless API clients — hence a far smaller cap than a typical HTTP
	// pool. Allowlisted deployments are small by construction.
	userPoolMaxUsers = 25
	// userPoolMaxPerUser caps how many concurrent assemblies ONE account may
	// hold. Because the pool is keyed by (user id, sid), a single account with
	// many authorizations (several clients, or stale sids from re-logins) could
	// otherwise fill every global slot and LRU-evict other users' live
	// assemblies. This confines that churn to the account causing it while
	// staying well above normal multi-client use.
	//
	// Exceeding it is NOT a hard rejection: a request for an over-cap session
	// evicts that same account's least-recently-used assembly and rebuilds the
	// requested one (a fresh TCP + MTProto handshake). An account round-robining
	// across more than this many sessions therefore pays a reconnect per switch.
	// 4 is chosen to sit above realistic device counts so that cost is not hit in
	// normal use; raise it if a deployment genuinely runs more clients per
	// account concurrently.
	userPoolMaxPerUser = 4
	// userPoolJanitorInterval is how often idle entries are collected.
	userPoolJanitorInterval = time.Minute
)

// errPoolFull is returned when the pool is at capacity with no idle entry.
var errPoolFull = errors.New("user pool is full")

// builtAssembly is the result of one per-authorization build. Handler serves MCP;
// Closer tears the assembly down on eviction (disconnecting the client);
// Health is a liveness probe returning nil while the assembly can serve and
// the fatal error once its Telegram client's Run loop has exited (session
// revoked remotely, transport death). Health is an explicit field rather than
// an optional interface on Handler so that forgetting the probe is a visible
// compile-site omission, not a silent "always healthy" downgrade. A nil
// Health means "always healthy".
type builtAssembly struct {
	Handler http.Handler
	Closer  io.Closer
	Health  func() error
}

// userHandlerBuilder builds the complete per-authorization HTTP assembly: MCP
// server(s) whose tools run on the user's own Telegram session.
type userHandlerBuilder func(ctx context.Context, user *authsrv.UserIdentity) (builtAssembly, error)

// deadReason returns the fatal error if a completed, successfully-built entry
// can no longer serve, or nil while it is healthy (or has no probe).
func (e *userEntry) deadReason() error {
	if !e.done() || e.buildErr != nil || e.health == nil {
		return nil
	}
	return e.health()
}

// userEntry is one user's pooled assembly.
//
// Lifecycle: building (ready open) → done (ready closed), where done is
// either ready-to-serve (buildErr nil, handler/closer set) or failed
// (buildErr set; the entry has already been removed from the pool map).
// finish/fail are the only two transitions; both are one-shot.
type userEntry struct {
	key      poolKey
	ready    chan struct{}
	handler  http.Handler
	closer   io.Closer
	health   func() error
	buildErr error
	lastUsed atomic.Int64 // unix nanos
	inflight atomic.Int64
	// closeRequested marks an entry that EvictSession removed while it was busy
	// or building; the last in-flight request closes it on release so a live
	// assembly is never torn down out from under an active request.
	closeRequested atomic.Bool
	// closeOnce guarantees the assembly's Closer runs exactly once no matter how
	// many teardown paths (evict, idle, revoke, shutdown) reach this entry.
	closeOnce sync.Once
}

// finish publishes a successful build. Field writes happen strictly before
// close(ready), which is the happens-before edge waiters rely on.
func (e *userEntry) finish(a builtAssembly) {
	e.handler = a.Handler
	e.closer = a.Closer
	e.health = a.Health
	close(e.ready)
}

// fail publishes a failed build.
func (e *userEntry) fail(err error) {
	e.buildErr = err
	close(e.ready)
}

// done reports whether the build has completed (successfully or not).
func (e *userEntry) done() bool {
	select {
	case <-e.ready:
		return true
	default:
		return false
	}
}

// evictable reports whether the entry can be torn down: build completed
// successfully and no request is using it. The whole eviction invariant
// lives here; both evictors must go through it.
func (e *userEntry) evictable() bool {
	return e.done() && e.buildErr == nil && e.inflight.Load() == 0
}

// poolKey identifies one pooled assembly: a Telegram user plus the
// per-authorization session id. Keying by both (rather than by user id alone)
// lets a single account hold several concurrent authorizations — each its own
// session object and its own live MTProto client — without contending. Named
// poolKey (not sessionKey) to avoid confusion with UserIdentity.SessionKey,
// which is the secret per-session encryption key and must never be logged; a
// poolKey (id + sid) is loggable.
type poolKey struct {
	id  tgid.UserID
	sid string
}

// poolKeyFor is the single derivation of a request's pool key from its
// identity, shared by entryFor and runBuild's cleanup so the two can never
// drift (a divergence would let a failed build's entry leak in the map).
func poolKeyFor(user *authsrv.UserIdentity) poolKey {
	return poolKey{id: user.ID, sid: user.SessionID}
}

// userPool caches one HTTP assembly per authorization, keyed by (user id,
// session id). Builds are singleflighted; failed builds are not cached; idle
// entries are evicted (and their Telegram clients disconnected) by the janitor.
type userPool struct {
	mu      sync.Mutex
	entries map[poolKey]*userEntry
	build   userHandlerBuilder
	// baseCtx is the server-lifetime context builds run on, so canceling one
	// request cannot poison a build other requests will share.
	baseCtx  context.Context
	maxUsers int
	now      func() time.Time
	logger   *slog.Logger
	// wwwAuthenticate is sent on pool-issued 401s (dead session) so MCP
	// clients discover the OAuth metadata and re-run the login flow, matching
	// the header RequireBearerToken sends on token failures.
	wwwAuthenticate string
	// testHookDeadBusyBeforeStore, when non-nil, is invoked in the dead-but-busy
	// eviction branch of entryFor immediately before closeRequested is stored.
	// It exists solely to let a test deterministically interpose the holder's
	// release() at the one instant that would expose a zero-close if the
	// post-store inflight recheck were ever removed. Always nil in production.
	testHookDeadBusyBeforeStore func()
}

func newUserPool(baseCtx context.Context, build userHandlerBuilder, wwwAuthenticate string, logger *slog.Logger) *userPool {
	return &userPool{
		entries:         map[poolKey]*userEntry{},
		build:           build,
		baseCtx:         baseCtx,
		maxUsers:        userPoolMaxUsers,
		now:             time.Now,
		logger:          logger,
		wwwAuthenticate: wwwAuthenticate,
	}
}

// ServeHTTP dispatches the request to the caller's assembly, building it on
// first use. It must run behind auth.RequireBearerToken, which is what
// populates the identity in the request context.
func (p *userPool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, ok := authsrv.Identity(r.Context())
	if !ok || user.ID <= 0 {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	entry, err := p.entryFor(r.Context(), user)
	if entry != nil {
		defer p.release(entry)
	}
	switch {
	case errors.Is(err, errPoolFull):
		p.logger.Warn("user pool at capacity, rejecting request", "user", user.ID, "cap", p.maxUsers)
		w.Header().Set("Retry-After", "60")
		http.Error(w, "server is at capacity, retry later", http.StatusServiceUnavailable)
		return
	case errors.Is(err, tgclient.ErrSessionUnauthorized):
		// The stored session is dead (revoked from Telegram's Devices menu,
		// or never completed). A 401 with resource metadata sends the MCP
		// client back through OAuth, whose QR login mints a fresh session.
		p.logger.Warn("telegram session unauthorized, forcing re-login", "user", user.ID)
		if p.wwwAuthenticate != "" {
			w.Header().Set("WWW-Authenticate", p.wwwAuthenticate)
		}
		http.Error(w, "telegram session is no longer authorized, please re-authenticate", http.StatusUnauthorized)
		return
	case err != nil && r.Context().Err() != nil:
		// The request context itself ended — the caller went away while
		// waiting for the build. Nothing to send. We check the request
		// context directly rather than sniffing context sentinels out of
		// err's chain: a build's own dial/handshake/GCS timeouts also wrap
		// context.DeadlineExceeded, and those must be logged, not swallowed.
		return
	case err != nil:
		p.logger.Error("building user assembly failed", "user", user.ID, "err", err)
		http.Error(w, "failed to initialize Telegram client", http.StatusServiceUnavailable)
		return
	}
	entry.handler.ServeHTTP(w, r)
}

// entryFor returns the caller's entry with inflight already incremented (the
// caller must release a non-nil entry), building the assembly on first use.
// Waiting on a concurrent build is bounded by ctx (the request context): a
// canceled caller stops waiting, while the build itself continues on the
// pool's base context for the next request to reuse.
func (p *userPool) entryFor(ctx context.Context, user *authsrv.UserIdentity) (*userEntry, error) {
	key := poolKeyFor(user)
	p.mu.Lock()
	if e, ok := p.entries[key]; ok {
		// A dead client (Run loop exited) must not serve. If nothing is
		// using the entry, evict and fall through to a fresh build; if
		// requests are still holding it (hung streams on the dead client),
		// map to the same 401 the dead-session build path produces — the
		// client re-authenticates and retries.
		if dead := e.deadReason(); dead != nil {
			if !e.evictable() {
				// Dead but still held by in-flight requests. Remove it so no new
				// request attaches, and flag it so the LAST in-flight release()
				// tears the dead assembly down promptly — otherwise it lingers
				// until a future request for this key or the idle janitor. This
				// request holds no inflight ref of its own (the increment lives in
				// the else branch), so the flag targets the actual holders.
				delete(p.entries, key)
				p.mu.Unlock()
				// Store the flag, THEN re-read inflight — the same order as
				// EvictSession and release, so with seq-cst atomics exactly one of
				// this branch and the last release() runs closeEntry (closeOnce
				// guards a tie). Without this recheck a release() that decremented to
				// 0 before our Store would observe the old flag and skip the close,
				// permanently leaking the now map-detached dead entry (its Closer,
				// hence the pinned-chat watcher goroutine, would never run).
				if p.testHookDeadBusyBeforeStore != nil {
					p.testHookDeadBusyBeforeStore()
				}
				e.closeRequested.Store(true)
				if e.done() && e.inflight.Load() == 0 {
					p.closeEntry(key, e)
				}
				p.logger.Warn("pooled Telegram client is down but still in use; forcing re-login", "user", key.id, "session", key.sid, "reason", dead)
				return nil, fmt.Errorf("pooled Telegram client is down: %w", tgclient.ErrSessionUnauthorized)
			}
			delete(p.entries, key)
			p.mu.Unlock()
			p.logger.Warn("evicting dead user assembly, rebuilding", "user", key.id, "session", key.sid, "reason", dead)
			p.closeEntry(key, e)
			p.mu.Lock()
			if _, raced := p.entries[key]; raced {
				// Another request started a rebuild while we were closing;
				// fall through to the normal cached-entry path on retry.
				p.mu.Unlock()
				return p.entryFor(ctx, user)
			}
		} else {
			e.inflight.Add(1)
			e.lastUsed.Store(p.now().UnixNano())
			p.mu.Unlock()
			select {
			case <-e.ready:
			case <-ctx.Done():
				return e, fmt.Errorf("waiting for user assembly build: %w", ctx.Err())
			}
			if e.buildErr != nil {
				return e, e.buildErr
			}
			return e, nil
		}
	}

	type eviction struct {
		key   poolKey
		entry *userEntry
		// reason is the log line for this eviction (per-account vs global cap);
		// both close identically.
		reason string
	}
	var evictions []eviction

	// Per-account cap first: evict THIS user's own LRU assembly, never another
	// user's, so one account cannot starve the pool. If the account is at its
	// cap with every entry busy, reject only this request.
	if p.countForUserLocked(user.ID) >= userPoolMaxPerUser {
		ek, ee := p.takeOneEvictableLocked(func(k poolKey) bool { return k.id == user.ID })
		if ee == nil {
			p.mu.Unlock()
			return nil, errPoolFull
		}
		evictions = append(evictions, eviction{ek, ee, "evicted account's own least-recently-used assembly (per-account cap)"})
	}

	// Global cap: evict the pool-wide LRU evictable entry.
	if len(p.entries) >= p.maxUsers {
		ek, ee := p.takeOneEvictableLocked(nil)
		if ee == nil {
			p.mu.Unlock()
			return nil, errPoolFull
		}
		evictions = append(evictions, eviction{ek, ee, "evicted least-recently-used user assembly to make room"})
	}
	e := &userEntry{key: key, ready: make(chan struct{})}
	e.inflight.Add(1)
	e.lastUsed.Store(p.now().UnixNano())
	p.entries[key] = e
	p.mu.Unlock()

	for _, ev := range evictions {
		p.logger.Info(ev.reason, "user", ev.key.id, "session", ev.key.sid)
		p.closeEntry(ev.key, ev.entry)
	}

	if err := p.runBuild(e, user); err != nil {
		return e, err
	}
	p.logger.Info("built MCP assembly", "user", user.ID, "session", key.sid, "pool_size", p.size())
	return e, nil
}

// runBuild executes the builder for a freshly inserted entry, converting
// panics into build errors. Whatever happens, the entry's ready channel is
// closed and failed entries are removed from the map — a builder panic must
// not leave waiters blocked forever with the janitor unable to intervene.
func (p *userPool) runBuild(e *userEntry, user *authsrv.UserIdentity) (retErr error) {
	completed := false
	defer func() {
		if completed {
			return
		}
		if r := recover(); r != nil {
			retErr = fmt.Errorf("user assembly build panic: %v", r)
			p.logger.Error("user assembly build panic", "user", user.ID, "panic", r, "stack", string(debug.Stack()))
		} else if retErr == nil {
			retErr = fmt.Errorf("user assembly build aborted")
		}
		e.fail(retErr)
		p.mu.Lock()
		// Delete only if the map still holds THIS entry: a concurrent
		// EvictSession (revoke/upgrade) may have already removed it and a new
		// request may have inserted a fresh entry under the same key, which must
		// not be clobbered by this failed build's cleanup.
		if p.entries[poolKeyFor(user)] == e {
			delete(p.entries, poolKeyFor(user))
		}
		p.mu.Unlock()
	}()

	asm, err := p.build(p.baseCtx, user)
	if err != nil {
		return err
	}
	e.finish(asm)
	completed = true
	return nil
}

// release decrements the entry's inflight counter and, if this was the last
// in-flight request on an entry EvictSession marked for teardown, closes it —
// the deferred half of "never Close() an assembly under an active request".
//
// Dereferencing e.closer here (via closeEntry) outside p.mu is safe only
// because the builder holds an inflight ref from entry creation until after
// finish() publishes closer: the inflight 0-transition therefore cannot happen
// while the entry is still building. A future change that detaches builds
// without holding inflight would break that and must revisit this.
func (p *userPool) release(e *userEntry) {
	if e.inflight.Add(-1) == 0 && e.closeRequested.Load() {
		p.closeEntry(e.key, e)
	}
}

func (p *userPool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// takeOneEvictableLocked removes and returns the least-recently-used evictable
// entry for which want returns true (want nil ⇒ any entry), or (zero, nil) when
// none qualifies. The caller closes the entry outside the pool lock. One helper
// serves both the global cap (want nil) and the per-account cap (want = same
// user), so the LRU scan invariant lives in exactly one place.
func (p *userPool) takeOneEvictableLocked(want func(poolKey) bool) (poolKey, *userEntry) {
	var oldestKey poolKey
	var oldest *userEntry
	for k, e := range p.entries {
		if !e.evictable() || (want != nil && !want(k)) {
			continue
		}
		if oldest == nil || e.lastUsed.Load() < oldest.lastUsed.Load() {
			oldestKey, oldest = k, e
		}
	}
	if oldest == nil {
		return poolKey{}, nil
	}
	delete(p.entries, oldestKey)
	return oldestKey, oldest
}

// countForUserLocked returns how many entries belong to one account.
func (p *userPool) countForUserLocked(id tgid.UserID) int {
	n := 0
	for k := range p.entries {
		if k.id == id {
			n++
		}
	}
	return n
}

// EvictSession drops the pooled assembly for one (userID, sid) session so its
// connection is freed after a revoke or legacy upgrade. It is best-effort
// cleanup, NOT the correctness mechanism: revocation is guaranteed by the
// durable tombstone (the refresh gate checks Revoked), so a live client that
// keeps running until its in-flight stream ends is harmless. It removes the
// entry from the map (no new request can attach) and closes it now if built and
// idle, else defers the close to the last in-flight request's release — an
// assembly is never Close()d out from under an active request.
func (p *userPool) EvictSession(userID tgid.UserID, sid string) {
	key := poolKey{id: userID, sid: sid}
	p.mu.Lock()
	e, ok := p.entries[key]
	if ok {
		delete(p.entries, key)
	}
	p.mu.Unlock()
	if !ok {
		return
	}
	// Order matters: set the flag first, then read inflight; release() mirrors
	// (decrement, then read the flag), so with seq-cst atomics exactly one of the
	// two runs closeEntry (closeOnce guards even that). A busy entry is closed by
	// the last in-flight request's release — never torn down under an active
	// request. This teardown is only about freeing the pooled connection;
	// revocation correctness comes from the durable tombstone, not from stopping
	// the client, so a brief live-client overlap here is harmless.
	e.closeRequested.Store(true)
	if e.done() && e.inflight.Load() == 0 {
		p.closeEntry(key, e)
	}
}

// janitor evicts idle entries until ctx is canceled.
func (p *userPool) janitor(ctx context.Context) {
	t := time.NewTicker(userPoolJanitorInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.evictIdle()
		}
	}
}

// evictIdle removes every evictable entry that has been idle past
// userPoolIdleTTL. In-flight requests (including hanging SSE streams) hold
// inflight > 0 and are never evicted. Teardown (disconnecting the Telegram
// client) runs after the pool lock is released so a slow close cannot stall
// other requests.
func (p *userPool) evictIdle() {
	cutoff := p.now().Add(-userPoolIdleTTL).UnixNano()
	type victim struct {
		key   poolKey
		entry *userEntry
	}
	var victims []victim

	p.mu.Lock()
	for k, e := range p.entries {
		if e.evictable() && e.lastUsed.Load() < cutoff {
			delete(p.entries, k)
			victims = append(victims, victim{k, e})
		}
	}
	p.mu.Unlock()

	for _, v := range victims {
		p.logger.Info("evicted idle user assembly", "user", v.key.id, "session", v.key.sid)
		p.closeEntry(v.key, v.entry)
	}
}

// closeEntry tears down an evicted entry's assembly. It is idempotent
// (closeOnce): several teardown paths may target the same entry, and every one
// funnels through here, so the underlying Closer runs at most once.
func (p *userPool) closeEntry(key poolKey, e *userEntry) {
	e.closeOnce.Do(func() {
		if e.closer == nil {
			return
		}
		if err := e.closer.Close(); err != nil {
			p.logger.Warn("closing evicted user assembly failed", "user", key.id, "session", key.sid, "err", err)
		}
	})
}

// Close tears down every pooled assembly. Called after the HTTP server's
// graceful drain; the drain is bounded (see serveHTTP), so requests that
// outlive it — hanging SSE streams at shutdown — are force-closed first and
// may observe a disconnected Telegram client. That is the accepted shutdown
// trade-off.
func (p *userPool) Close() error {
	p.mu.Lock()
	entries := p.entries
	p.entries = map[poolKey]*userEntry{}
	p.mu.Unlock()

	var errs []error
	for _, e := range entries {
		// Flag every entry, including ones still building. A build that completes
		// after this snapshot publishes a live client that Close would otherwise
		// leak (its creating request is blocked in the synchronous runBuild; on
		// return its release() sees the flag and closes via closeOnce). Done
		// entries are force-closed below regardless.
		e.closeRequested.Store(true)
		if !e.done() {
			continue
		}
		// Route through closeOnce so an entry a concurrent EvictSession or a
		// post-build release() already closed is never Close()d twice.
		e.closeOnce.Do(func() {
			if e.closer != nil {
				if err := e.closer.Close(); err != nil {
					errs = append(errs, err)
				}
			}
		})
	}
	return errors.Join(errs...)
}

// multiCloser closes several closers as one, joining errors.
type multiCloser []io.Closer

func (m multiCloser) Close() error {
	var errs []error
	for _, c := range m {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
}

// closerFunc adapts a function to io.Closer.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }
