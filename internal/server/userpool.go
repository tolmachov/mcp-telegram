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
	"time"

	"golang.org/x/time/rate"

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
	// userPoolEvictGrace is the default evictGrace (see the userPool field).
	userPoolEvictGrace = time.Minute
	initializeBurst    = 3
)

var initializeRate = rate.Every(time.Minute / 10)

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

type userEntryState uint8

const (
	entryBuilding userEntryState = iota
	entryBuildingEvicted
	entryActive
	entryDraining
	entryFailed
	entryClosed
)

// userEntry is one user's pooled assembly.
//
// Every field except ready is protected by userPool.mu. The ready channel is
// only closed while holding that mutex and is the publication edge for build
// results. Lifecycle transitions are centralized in completeBuildLocked,
// evictLocked, release and Close.
type userEntry struct {
	key           poolKey
	ready         chan struct{}
	state         userEntryState
	handler       http.Handler
	closer        io.Closer
	health        func() error
	buildErr      error
	lastUsed      time.Time
	inflight      int
	evictDeadline time.Time
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
	mu                 sync.Mutex
	entries            map[poolKey]*userEntry
	retired            map[*userEntry]struct{}
	initializeLimiters map[tgid.UserID]*rate.Limiter
	build              userHandlerBuilder
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
	// evictGrace bounds how long a busy revoked assembly may drain. A single
	// janitor timer handles every deadline; entries never create competing
	// time.AfterFunc callbacks.
	evictGrace time.Duration
	wake       chan struct{}
}

func newUserPool(baseCtx context.Context, build userHandlerBuilder, wwwAuthenticate string, logger *slog.Logger) *userPool {
	return &userPool{
		entries:            map[poolKey]*userEntry{},
		retired:            map[*userEntry]struct{}{},
		initializeLimiters: map[tgid.UserID]*rate.Limiter{},
		build:              build,
		baseCtx:            baseCtx,
		maxUsers:           userPoolMaxUsers,
		now:                time.Now,
		logger:             logger,
		wwwAuthenticate:    wwwAuthenticate,
		evictGrace:         userPoolEvictGrace,
		wake:               make(chan struct{}, 1),
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
	p.serveUser(w, r, user)
}

// serveUser dispatches a request whose authentication boundary has already
// produced a typed Telegram identity. Keeping identity extraction separate
// makes the pool state machine independently testable without any production
// token-forging hook.
func (p *userPool) serveUser(w http.ResponseWriter, r *http.Request, user *authsrv.UserIdentity) {
	if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") == "" && !p.allowInitialize(user.ID) {
		p.logger.Warn("sessionless MCP initialize rate limited", "user", user.ID)
		w.Header().Set("Retry-After", "6")
		http.Error(w, "too many new MCP sessions", http.StatusTooManyRequests)
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

func (p *userPool) allowInitialize(userID tgid.UserID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	limiter := p.initializeLimiters[userID]
	if limiter == nil {
		limiter = rate.NewLimiter(initializeRate, initializeBurst)
		p.initializeLimiters[userID] = limiter
	}
	return limiter.Allow()
}

// entryFor returns the caller's entry with inflight already incremented (the
// caller must release a non-nil entry), building the assembly on first use.
// Waiting on a concurrent build is bounded by ctx (the request context): a
// canceled caller stops waiting, while the build itself continues on the
// pool's base context for the next request to reuse.
func (p *userPool) entryFor(ctx context.Context, user *authsrv.UserIdentity) (*userEntry, error) {
	key := poolKeyFor(user)
	for {
		type eviction struct {
			key    poolKey
			closer io.Closer
			reason string
		}
		var evictions []eviction

		p.mu.Lock()
		if e := p.entries[key]; e != nil {
			if e.state == entryActive && e.health != nil {
				if dead := e.health(); dead != nil {
					busy := e.inflight > 0
					closer := p.evictLocked(e, false)
					p.mu.Unlock()
					p.closeAssembly(key, closer)
					if busy {
						p.logger.Warn("pooled Telegram client is down but still in use; forcing re-login", "user", key.id, "session", key.sid, "reason", dead)
						return nil, fmt.Errorf("pooled Telegram client is down: %w", tgclient.ErrSessionUnauthorized)
					}
					p.logger.Warn("evicting dead user assembly, rebuilding", "user", key.id, "session", key.sid, "reason", dead)
					continue
				}
			}
			if e.state != entryBuilding && e.state != entryActive {
				// Detached states must never remain addressable, but recover by
				// removing the stale map entry rather than attaching a request.
				delete(p.entries, key)
				p.mu.Unlock()
				continue
			}
			e.inflight++
			e.lastUsed = p.now()
			p.mu.Unlock()
			select {
			case <-e.ready:
			case <-ctx.Done():
				return e, fmt.Errorf("waiting for user assembly build: %w", ctx.Err())
			}
			p.mu.Lock()
			buildErr := e.buildErr
			p.mu.Unlock()
			if buildErr != nil {
				return e, buildErr
			}
			return e, nil
		}

		if p.countForUserLocked(user.ID) >= userPoolMaxPerUser {
			ek, closer, ok := p.takeOneEvictableLocked(func(k poolKey) bool { return k.id == user.ID })
			if !ok {
				p.mu.Unlock()
				return nil, errPoolFull
			}
			evictions = append(evictions, eviction{ek, closer, "evicted account's own least-recently-used assembly (per-account cap)"})
		}
		if len(p.entries) >= p.maxUsers {
			ek, closer, ok := p.takeOneEvictableLocked(nil)
			if !ok {
				p.mu.Unlock()
				for _, ev := range evictions {
					p.closeAssembly(ev.key, ev.closer)
				}
				return nil, errPoolFull
			}
			evictions = append(evictions, eviction{ek, closer, "evicted least-recently-used user assembly to make room"})
		}
		e := &userEntry{key: key, ready: make(chan struct{}), state: entryBuilding, inflight: 1, lastUsed: p.now()}
		p.entries[key] = e
		p.mu.Unlock()

		for _, ev := range evictions {
			p.logger.Info(ev.reason, "user", ev.key.id, "session", ev.key.sid)
			p.closeAssembly(ev.key, ev.closer)
		}
		if err := p.runBuild(e, user); err != nil {
			return e, err
		}
		p.logger.Info("built MCP assembly", "user", user.ID, "session", key.sid, "pool_size", p.size())
		return e, nil
	}
}

// runBuild executes the builder for a freshly inserted entry, converting
// panics into build errors. Whatever happens, the entry's ready channel is
// closed and failed entries are removed from the map — a builder panic must
// not leave waiters blocked forever with the janitor unable to intervene.
func (p *userPool) runBuild(e *userEntry, user *authsrv.UserIdentity) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("user assembly build panic: %v", r)
			p.logger.Error("user assembly build panic", "user", user.ID, "panic", r, "stack", string(debug.Stack()))
		}
		if retErr != nil {
			p.completeBuild(e, builtAssembly{}, retErr)
		}
	}()

	asm, err := p.build(p.baseCtx, user)
	if err != nil {
		return err
	}
	p.completeBuild(e, asm, nil)
	return nil
}

func (p *userPool) completeBuild(e *userEntry, asm builtAssembly, buildErr error) {
	p.mu.Lock()
	if buildErr != nil {
		e.buildErr = buildErr
		e.state = entryFailed
		if p.entries[e.key] == e {
			delete(p.entries, e.key)
		}
		delete(p.retired, e)
		close(e.ready)
		p.mu.Unlock()
		return
	}
	e.handler, e.closer, e.health = asm.Handler, asm.Closer, asm.Health
	if e.state == entryBuildingEvicted {
		e.state = entryDraining
		e.evictDeadline = p.now().Add(p.evictGrace)
		p.retired[e] = struct{}{}
	} else {
		e.state = entryActive
	}
	close(e.ready)
	p.mu.Unlock()
	p.signalJanitor()
}

// release is the only request-release transition. It closes a detached entry
// exactly once when the last holder leaves; no atomics or close-once races are
// involved because both the count and state change under p.mu.
func (p *userPool) release(e *userEntry) {
	p.mu.Lock()
	if e.inflight > 0 {
		e.inflight--
	}
	var closer io.Closer
	if e.inflight == 0 && e.state == entryDraining {
		closer = p.finalizeLocked(e)
	}
	p.mu.Unlock()
	p.closeAssembly(e.key, closer)
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
func (p *userPool) takeOneEvictableLocked(want func(poolKey) bool) (poolKey, io.Closer, bool) {
	var oldestKey poolKey
	var oldest *userEntry
	for k, e := range p.entries {
		if e.state != entryActive || e.inflight != 0 || (want != nil && !want(k)) {
			continue
		}
		if oldest == nil || e.lastUsed.Before(oldest.lastUsed) {
			oldestKey, oldest = k, e
		}
	}
	if oldest == nil {
		return poolKey{}, nil, false
	}
	return oldestKey, p.evictLocked(oldest, false), true
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
// connection is freed after a revoke. It is best-effort
// cleanup, NOT the correctness mechanism: revocation is guaranteed by the
// durable tombstone (the refresh gate checks Revoked). It removes the entry
// from the map immediately. Idle entries close synchronously; busy entries
// drain until their last release or the centralized janitor reaches evictGrace.
func (p *userPool) EvictSession(userID tgid.UserID, sid string) {
	key := poolKey{id: userID, sid: sid}
	p.mu.Lock()
	e := p.entries[key]
	closer := p.evictLocked(e, false)
	p.mu.Unlock()
	p.closeAssembly(key, closer)
	p.signalJanitor()
}

// janitor owns the pool's only eviction timer. Wakes recalculate the nearest
// retired-entry deadline, so changing pool state never creates per-entry timer
// callbacks that can race release or shutdown.
func (p *userPool) janitor(ctx context.Context) {
	for {
		delay := p.nextSweepDelay()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
			p.evictIdle()
		case <-p.wake:
			if !timer.Stop() {
				<-timer.C
			}
		}
	}
}

func (p *userPool) signalJanitor() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *userPool) nextSweepDelay() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	delay := userPoolJanitorInterval
	for e := range p.retired {
		if e.state != entryDraining || e.evictDeadline.IsZero() {
			continue
		}
		until := e.evictDeadline.Sub(now)
		if until <= 0 {
			return time.Nanosecond
		}
		if until < delay {
			delay = until
		}
	}
	return delay
}

// evictIdle removes every evictable entry that has been idle past
// userPoolIdleTTL. In-flight requests (including hanging SSE streams) hold
// inflight > 0 and are never evicted. Teardown (disconnecting the Telegram
// client) runs after the pool lock is released so a slow close cannot stall
// other requests.
func (p *userPool) evictIdle() {
	type victim struct {
		key    poolKey
		closer io.Closer
	}
	var victims []victim

	p.mu.Lock()
	now := p.now()
	cutoff := now.Add(-userPoolIdleTTL)
	for k, e := range p.entries {
		if e.state == entryActive && e.inflight == 0 && e.lastUsed.Before(cutoff) {
			victims = append(victims, victim{k, p.evictLocked(e, false)})
		}
	}
	for e := range p.retired {
		if e.state == entryDraining && !e.evictDeadline.IsZero() && !now.Before(e.evictDeadline) {
			victims = append(victims, victim{e.key, p.finalizeLocked(e)})
		}
	}
	p.mu.Unlock()

	for _, v := range victims {
		p.logger.Info("evicted user assembly", "user", v.key.id, "session", v.key.sid)
		p.closeAssembly(v.key, v.closer)
	}
}

func (p *userPool) evictLocked(e *userEntry, force bool) io.Closer {
	if e == nil || e.state == entryClosed || e.state == entryFailed {
		return nil
	}
	if p.entries[e.key] == e {
		delete(p.entries, e.key)
	}
	switch e.state {
	case entryBuilding:
		e.state = entryBuildingEvicted
		p.retired[e] = struct{}{}
		return nil
	case entryBuildingEvicted:
		return nil
	case entryActive:
		if !force && e.inflight > 0 {
			e.state = entryDraining
			e.evictDeadline = p.now().Add(p.evictGrace)
			p.retired[e] = struct{}{}
			return nil
		}
		return p.finalizeLocked(e)
	case entryDraining:
		if force || e.inflight == 0 {
			return p.finalizeLocked(e)
		}
	}
	return nil
}

func (p *userPool) finalizeLocked(e *userEntry) io.Closer {
	if e == nil || e.state == entryClosed {
		return nil
	}
	e.state = entryClosed
	delete(p.retired, e)
	return e.closer
}

func (p *userPool) closeAssembly(key poolKey, closer io.Closer) {
	if closer == nil {
		return
	}
	if err := closer.Close(); err != nil {
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
	all := make(map[*userEntry]struct{}, len(p.entries)+len(p.retired))
	for _, e := range p.entries {
		all[e] = struct{}{}
	}
	for e := range p.retired {
		all[e] = struct{}{}
	}
	p.entries = map[poolKey]*userEntry{}
	var closers []io.Closer
	for e := range all {
		if closer := p.evictLocked(e, true); closer != nil {
			closers = append(closers, closer)
		}
	}
	p.mu.Unlock()

	var errs []error
	for _, closer := range closers {
		if err := closer.Close(); err != nil {
			errs = append(errs, err)
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
