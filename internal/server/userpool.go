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
	// userPoolJanitorInterval is how often idle entries are collected.
	userPoolJanitorInterval = time.Minute
)

// errPoolFull is returned when the pool is at capacity with no idle entry.
var errPoolFull = errors.New("user pool is full")

// builtAssembly is the result of one per-user build. Handler serves MCP;
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

// userHandlerBuilder builds the complete per-user HTTP assembly: MCP
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
	ready    chan struct{}
	handler  http.Handler
	closer   io.Closer
	health   func() error
	buildErr error
	lastUsed atomic.Int64 // unix nanos
	inflight atomic.Int64
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

// sessionKey identifies one pooled assembly: a Telegram user plus the
// per-authorization session id. Keying by both (rather than by user id alone)
// lets a single account hold several concurrent authorizations — each its own
// session object and its own live MTProto client — without contending.
type sessionKey struct {
	id  tgid.UserID
	sid string
}

// userPool caches one HTTP assembly per authorization, keyed by (user id,
// session id). Builds are singleflighted; failed builds are not cached; idle
// entries are evicted (and their Telegram clients disconnected) by the janitor.
type userPool struct {
	mu      sync.Mutex
	entries map[sessionKey]*userEntry
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
}

func newUserPool(baseCtx context.Context, build userHandlerBuilder, wwwAuthenticate string, logger *slog.Logger) *userPool {
	return &userPool{
		entries:         map[sessionKey]*userEntry{},
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
	key := sessionKey{id: user.ID, sid: user.SessionID}
	p.mu.Lock()
	if e, ok := p.entries[key]; ok {
		// A dead client (Run loop exited) must not serve. If nothing is
		// using the entry, evict and fall through to a fresh build; if
		// requests are still holding it (hung streams on the dead client),
		// map to the same 401 the dead-session build path produces — the
		// client re-authenticates and retries.
		if dead := e.deadReason(); dead != nil {
			if !e.evictable() {
				p.mu.Unlock()
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

	var evicted *userEntry
	var evictedKey sessionKey
	if len(p.entries) >= p.maxUsers {
		evictedKey, evicted = p.takeOneEvictableLocked()
		if evicted == nil {
			p.mu.Unlock()
			return nil, errPoolFull
		}
	}
	e := &userEntry{ready: make(chan struct{})}
	e.inflight.Add(1)
	e.lastUsed.Store(p.now().UnixNano())
	p.entries[key] = e
	p.mu.Unlock()

	if evicted != nil {
		p.logger.Info("evicted least-recently-used user assembly to make room", "user", evictedKey.id, "session", evictedKey.sid)
		p.closeEntry(evictedKey, evicted)
	}

	if err := p.runBuild(e, user); err != nil {
		return e, err
	}
	p.logger.Info("built per-user MCP assembly", "user", user.ID, "pool_size", p.size())
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
		delete(p.entries, sessionKey{id: user.ID, sid: user.SessionID})
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

// release decrements the entry's inflight counter.
func (p *userPool) release(e *userEntry) {
	e.inflight.Add(-1)
}

func (p *userPool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// takeOneEvictableLocked removes and returns the least-recently-used
// evictable entry, or nil when every entry is busy or still building. The
// caller closes it outside the pool lock.
func (p *userPool) takeOneEvictableLocked() (sessionKey, *userEntry) {
	var oldestKey sessionKey
	var oldest *userEntry
	for k, e := range p.entries {
		if !e.evictable() {
			continue
		}
		if oldest == nil || e.lastUsed.Load() < oldest.lastUsed.Load() {
			oldestKey, oldest = k, e
		}
	}
	if oldest == nil {
		return sessionKey{}, nil
	}
	delete(p.entries, oldestKey)
	return oldestKey, oldest
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
		key   sessionKey
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

// closeEntry tears down an evicted entry's assembly.
func (p *userPool) closeEntry(key sessionKey, e *userEntry) {
	if e.closer == nil {
		return
	}
	if err := e.closer.Close(); err != nil {
		p.logger.Warn("closing evicted user assembly failed", "user", key.id, "session", key.sid, "err", err)
	}
}

// Close tears down every pooled assembly. Called after the HTTP server's
// graceful drain; the drain is bounded (see serveHTTP), so requests that
// outlive it — hanging SSE streams at shutdown — are force-closed first and
// may observe a disconnected Telegram client. That is the accepted shutdown
// trade-off.
func (p *userPool) Close() error {
	p.mu.Lock()
	entries := p.entries
	p.entries = map[sessionKey]*userEntry{}
	p.mu.Unlock()

	var errs []error
	for _, e := range entries {
		if e.done() && e.closer != nil {
			errs = append(errs, e.closer.Close())
		}
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
