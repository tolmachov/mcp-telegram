package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

const testWWWAuthenticate = `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"`

// waitFor polls cond until it holds or a short deadline elapses, failing the
// test on timeout. Used to synchronise on concurrent pool state (e.g. a waiter
// attaching to an in-flight build) without racy fixed sleeps.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within deadline")
		}
		time.Sleep(time.Millisecond)
	}
}

// poolRequest sends one request through the pool behind a stub bearer
// verifier, so the identity reaches the pool exactly the way the production
// RequireBearerToken middleware delivers it. Use poolRequestSID for explicit
// per-authorization identities.
func poolRequest(t *testing.T, p *userPool, id tgid.UserID) *httptest.ResponseRecorder {
	return poolRequestSID(t, p, id, "")
}

// poolRequestSID is poolRequest with an explicit per-authorization session id,
// so tests can drive several independent sessions of one account.
func poolRequestSID(t *testing.T, p *userPool, id tgid.UserID, sid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Mcp-Session-Id", "existing-test-session")
	rec := httptest.NewRecorder()
	p.serveUser(rec, req, &authsrv.UserIdentity{ID: id, Username: "u", SessionID: sid})
	return rec
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestUserPoolServeHTTPRejectsMissingIdentity(t *testing.T) {
	pool := newUserPool(t.Context(), func(context.Context, *authsrv.UserIdentity) (builtAssembly, error) {
		t.Fatal("builder must not run for an unauthenticated request")
		return builtAssembly{}, nil
	}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())
	recorder := httptest.NewRecorder()
	pool.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestUserPoolLimitsSessionlessInitializePerUser(t *testing.T) {
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		return okAssembly(), nil
	}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())
	for i := range initializeBurst + 1 {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		rec := httptest.NewRecorder()
		pool.serveUser(rec, req, &authsrv.UserIdentity{ID: 7, Username: "u", SessionID: "sid"})
		if i < initializeBurst && rec.Code != http.StatusOK {
			t.Fatalf("sessionless request %d status = %d, want 200", i+1, rec.Code)
		}
		if i == initializeBurst && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("sessionless request %d status = %d, want 429", i+1, rec.Code)
		}
	}
}

// okAssembly is a healthy build with a no-op closer.
func okAssembly() builtAssembly {
	return builtAssembly{Handler: okHandler(), Closer: closerFunc(func() error { return nil }), Health: healthy}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// healthy is the probe of an assembly whose client never stops.
func healthy() error { return nil }

// unexpectedDrop fails the test if the pool deletes a stored session.
func unexpectedDrop(t *testing.T) sessionDropper {
	return func(_ context.Context, id tgid.UserID, sid string) error {
		t.Errorf("pool deleted session (%d, %q); no session was refused", id, sid)
		return nil
	}
}

// dropRecorder records the stored sessions the pool deletes.
type dropRecorder struct {
	mu      sync.Mutex
	dropped []poolKey
}

func (d *dropRecorder) drop(_ context.Context, id tgid.UserID, sid string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dropped = append(d.dropped, poolKey{id: id, sid: sid})
	return nil
}

func (d *dropRecorder) keys() []poolKey {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]poolKey(nil), d.dropped...)
}

func TestUserPoolBuildsOncePerUser(t *testing.T) {
	var builds atomic.Int64
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		builds.Add(1)
		return okAssembly(), nil
	}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())

	for range 3 {
		if rec := poolRequest(t, pool, 1); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	}
	if rec := poolRequest(t, pool, 2); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := builds.Load(); got != 2 {
		t.Errorf("builder ran %d times, want 2 (one per user)", got)
	}
}

// TestUserPoolIndependentSessionsPerUser pins the core of the independent-
// session design: two authorizations of the SAME account (same user id,
// distinct session ids) get two separate assemblies and both serve — no
// contention, no forced re-login. A third request reusing the first session id
// must hit the warm entry rather than build again.
func TestUserPoolIndependentSessionsPerUser(t *testing.T) {
	var builds atomic.Int64
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		builds.Add(1)
		return okAssembly(), nil
	}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())

	if rec := poolRequestSID(t, pool, 1, "aaaa"); rec.Code != http.StatusOK {
		t.Fatalf("session A status = %d, want 200", rec.Code)
	}
	if rec := poolRequestSID(t, pool, 1, "bbbb"); rec.Code != http.StatusOK {
		t.Fatalf("session B status = %d, want 200", rec.Code)
	}
	// Reusing session A hits the warm assembly, not a new build.
	if rec := poolRequestSID(t, pool, 1, "aaaa"); rec.Code != http.StatusOK {
		t.Fatalf("session A reuse status = %d, want 200", rec.Code)
	}
	if got := builds.Load(); got != 2 {
		t.Errorf("builder ran %d times, want 2 (one per independent session)", got)
	}
	if pool.size() != 2 {
		t.Errorf("pool size = %d, want 2 (two independent sessions of one account)", pool.size())
	}
}

// TestUserPoolEvictSession pins that EvictSession tears down a specific
// session's assembly (even though it is idle-but-warm) so a revoked
// session's client is stopped and the next request cold-rebuilds.
func TestUserPoolEvictSession(t *testing.T) {
	var builds, closes atomic.Int64
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		builds.Add(1)
		return builtAssembly{
			Handler: okHandler(),
			Closer:  closerFunc(func() error { closes.Add(1); return nil }),
			Health:  healthy,
		}, nil
	}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())

	if rec := poolRequestSID(t, pool, 1, "aaaa"); rec.Code != http.StatusOK {
		t.Fatalf("build status = %d, want 200", rec.Code)
	}
	pool.EvictSession(1, "aaaa")
	if got := closes.Load(); got != 1 {
		t.Errorf("evicted session closed %d times, want 1", got)
	}
	if pool.size() != 0 {
		t.Errorf("pool size after EvictSession = %d, want 0", pool.size())
	}
	// A different session of the same user is untouched by the targeted evict.
	if rec := poolRequestSID(t, pool, 1, "aaaa"); rec.Code != http.StatusOK {
		t.Fatalf("rebuild status = %d, want 200", rec.Code)
	}
	if got := builds.Load(); got != 2 {
		t.Errorf("builder ran %d times, want 2 (rebuild after evict)", got)
	}
}

// TestUserPoolEvictSessionDefersBusyClose pins that EvictSession never closes an
// assembly that a request is still using: a busy entry is removed from the map
// immediately but closed only when the last in-flight request releases it.
func TestUserPoolEvictSessionDefersBusyClose(t *testing.T) {
	var closes atomic.Int64
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		return builtAssembly{
			Handler: okHandler(),
			Closer:  closerFunc(func() error { closes.Add(1); return nil }),
			Health:  healthy,
		}, nil
	}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())

	if rec := poolRequestSID(t, pool, 1, "aaaa"); rec.Code != http.StatusOK {
		t.Fatalf("build status = %d, want 200", rec.Code)
	}
	// Simulate an in-flight request holding the entry.
	pool.mu.Lock()
	e := pool.entries[poolKey{id: 1, sid: "aaaa"}]
	e.inflight++
	pool.mu.Unlock()

	pool.EvictSession(1, "aaaa")
	if got := closes.Load(); got != 0 {
		t.Errorf("busy entry closed %d times during evict; want 0 (deferred)", got)
	}
	if pool.size() != 0 {
		t.Errorf("entry not removed from map on evict; size = %d, want 0", pool.size())
	}
	// The last in-flight request releasing closes it exactly once.
	pool.release(e)
	if got := closes.Load(); got != 1 {
		t.Errorf("entry closed %d times after release; want 1", got)
	}
}

// TestUserPoolPerUserCap pins that one account cannot fill the pool: once it is
// at userPoolMaxPerUser assemblies, a further authorization evicts THAT
// account's own LRU entry, never another user's.
func TestUserPoolPerUserCap(t *testing.T) {
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		return okAssembly(), nil
	}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())

	// Another user's assembly must survive the churn below.
	if rec := poolRequestSID(t, pool, 2, "bbbb"); rec.Code != http.StatusOK {
		t.Fatalf("user 2 status = %d, want 200", rec.Code)
	}
	// User 1 opens one more authorization than its per-account cap.
	for i := 0; i <= userPoolMaxPerUser; i++ {
		sid := "aaaa" + string(rune('a'+i))
		if rec := poolRequestSID(t, pool, 1, sid); rec.Code != http.StatusOK {
			t.Fatalf("user 1 sid %s status = %d, want 200", sid, rec.Code)
		}
	}
	// User 1 is capped; user 2's single entry still present → total cap+1.
	if got := pool.size(); got != userPoolMaxPerUser+1 {
		t.Errorf("pool size = %d, want %d (user1 capped + user2)", got, userPoolMaxPerUser+1)
	}
	pool.mu.Lock()
	_, user2Present := pool.entries[poolKey{id: 2, sid: "bbbb"}]
	user1Count := pool.countForUserLocked(1)
	pool.mu.Unlock()
	if !user2Present {
		t.Error("user 2's assembly was evicted by user 1's churn; per-account cap must not touch other users")
	}
	if user1Count != userPoolMaxPerUser {
		t.Errorf("user 1 holds %d assemblies, want %d (its cap)", user1Count, userPoolMaxPerUser)
	}
}

// TestUserPoolRefusedAtBuildDeletesSessionAnd401s pins the startup half of a
// refused session: the stored session is deleted and the request answered
// 401 with the OAuth metadata, so the client signs in again.
func TestUserPoolRefusedAtBuildDeletesSessionAnd401s(t *testing.T) {
	drops := &dropRecorder{}
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		return builtAssembly{}, fmt.Errorf("connecting: %w", tgclient.ErrSessionUnauthorized)
	}, drops.drop, testWWWAuthenticate, discardLogger())

	rec := poolRequestSID(t, pool, 7, "dead")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != testWWWAuthenticate {
		t.Errorf("WWW-Authenticate = %q, want %q", got, testWWWAuthenticate)
	}
	if got := drops.keys(); len(got) != 1 || got[0] != (poolKey{id: 7, sid: "dead"}) {
		t.Errorf("dropped sessions = %v, want exactly (7, dead)", got)
	}
}

// TestUserPoolBoundsTheRefusedSessionDelete pins that deleting a refused
// session runs under sessionDropTimeout, so a stalled store still lets the
// request's 401 through, and that a failed delete does not change the answer.
func TestUserPoolBoundsTheRefusedSessionDelete(t *testing.T) {
	var bounded bool
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		return builtAssembly{}, fmt.Errorf("connecting: %w", tgclient.ErrSessionUnauthorized)
	}, func(ctx context.Context, _ tgid.UserID, _ string) error {
		deadline, ok := ctx.Deadline()
		bounded = ok && time.Until(deadline) <= sessionDropTimeout
		return errors.New("bucket unavailable")
	}, testWWWAuthenticate, discardLogger())

	if rec := poolRequestSID(t, pool, 7, "dead"); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if !bounded {
		t.Error("the delete must run under a deadline no later than sessionDropTimeout")
	}
}

func TestUserPoolBuildFailureNotCached(t *testing.T) {
	var builds atomic.Int64
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		if builds.Add(1) == 1 {
			return builtAssembly{}, errors.New("transient failure")
		}
		return okAssembly(), nil
	}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())

	if rec := poolRequest(t, pool, 5); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first request status = %d, want 503", rec.Code)
	}
	if rec := poolRequest(t, pool, 5); rec.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200 (failed build must not be cached)", rec.Code)
	}
}

func TestUserPoolFullRejectsWith503(t *testing.T) {
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		return okAssembly(), nil
	}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())

	for id := tgid.UserID(1); id <= userPoolMaxUsers; id++ {
		if rec := poolRequest(t, pool, id); rec.Code != http.StatusOK {
			t.Fatalf("user %d status = %d, want 200", id, rec.Code)
		}
	}
	// Every entry is idle (inflight 0) → LRU eviction makes room.
	if rec := poolRequest(t, pool, userPoolMaxUsers+1); rec.Code != http.StatusOK {
		t.Fatalf("user past the cap status = %d, want 200 after LRU eviction", rec.Code)
	}
	if pool.size() != userPoolMaxUsers {
		t.Errorf("pool size = %d, want %d", pool.size(), userPoolMaxUsers)
	}
}

func TestUserPoolRebuildsDeadAssembly(t *testing.T) {
	var builds atomic.Int64
	var closes atomic.Int64
	var deads []*atomic.Bool
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		builds.Add(1)
		dead := &atomic.Bool{}
		deads = append(deads, dead)
		return builtAssembly{
			Handler: okHandler(),
			Closer:  closerFunc(func() error { closes.Add(1); return nil }),
			Health: func() error {
				if dead.Load() {
					return errors.New("client run loop exited")
				}
				return nil
			},
		}, nil
	}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())

	if rec := poolRequest(t, pool, 1); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}
	deads[0].Store(true)

	// The dead assembly must be evicted, closed, and rebuilt transparently.
	if rec := poolRequest(t, pool, 1); rec.Code != http.StatusOK {
		t.Fatalf("request after client death status = %d, want 200 (rebuild)", rec.Code)
	}
	if got := builds.Load(); got != 2 {
		t.Errorf("builder ran %d times, want 2", got)
	}
	if got := closes.Load(); got != 1 {
		t.Errorf("dead assembly closed %d times, want 1", got)
	}
}

// stoppable is an assembly probe whose client can be stopped with a reason.
type stoppable struct {
	mu     sync.Mutex
	reason error
}

func (c *stoppable) stop(reason error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reason = reason
}

func (c *stoppable) health() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reason
}

// TestUserPoolRefusedMidRunDeletesSessionAnd401s pins the running half of a
// refused session: once Telegram refuses the session in reply to a call, the
// client's health says so and the next request evicts the assembly, deletes
// the stored session and answers 401 — without building a client on the dead
// session again — whether or not an older request still holds the assembly.
func TestUserPoolRefusedMidRunDeletesSessionAnd401s(t *testing.T) {
	for _, busy := range []bool{false, true} {
		var builds, closes atomic.Int64
		client := &stoppable{}
		drops := &dropRecorder{}
		pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
			builds.Add(1)
			return builtAssembly{
				Handler: okHandler(),
				Closer:  closerFunc(func() error { closes.Add(1); return nil }),
				Health:  client.health,
			}, nil
		}, drops.drop, testWWWAuthenticate, discardLogger())

		if rec := poolRequestSID(t, pool, 1, "s"); rec.Code != http.StatusOK {
			t.Fatalf("first request status = %d, want 200", rec.Code)
		}
		key := poolKey{id: 1, sid: "s"}
		pool.mu.Lock()
		e := pool.entries[key]
		if busy {
			e.inflight++
		}
		pool.mu.Unlock()
		client.stop(fmt.Errorf("%w: %w", tgclient.ErrSessionUnauthorized, tgerr.New(401, "SESSION_REVOKED")))

		rec := poolRequestSID(t, pool, 1, "s")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("busy=%v: status = %d, want 401", busy, rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != testWWWAuthenticate {
			t.Errorf("busy=%v: WWW-Authenticate = %q, want %q", busy, got, testWWWAuthenticate)
		}
		if got := drops.keys(); len(got) != 1 || got[0] != key {
			t.Errorf("busy=%v: dropped sessions = %v, want exactly %v", busy, got, key)
		}
		if got := builds.Load(); got != 1 {
			t.Errorf("busy=%v: builder ran %d times, want 1 (no client on a refused session)", busy, got)
		}
		if pool.size() != 0 {
			t.Errorf("busy=%v: pool size = %d, want 0", busy, pool.size())
		}
		if busy {
			if got := closes.Load(); got != 0 {
				t.Errorf("assembly closed %d times while still held; want 0 (deferred to release)", got)
			}
			pool.release(e)
		}
		if got := closes.Load(); got != 1 {
			t.Errorf("busy=%v: assembly closed %d times, want 1", busy, got)
		}
	}
}

// TestUserPoolStoppedClientIsRebuiltNotReauthenticated pins the pool's
// classification by cause: a client that stopped for any reason other than a
// refused session — a dropped connection, a transport failure — is evicted
// and rebuilt on the same session, never answered 401, and its session is
// kept, whether or not an older request still holds the assembly.
func TestUserPoolStoppedClientIsRebuiltNotReauthenticated(t *testing.T) {
	for _, busy := range []bool{false, true} {
		var builds, closes atomic.Int64
		var clients []*stoppable
		pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
			builds.Add(1)
			client := &stoppable{}
			clients = append(clients, client)
			return builtAssembly{
				Handler: okHandler(),
				Closer:  closerFunc(func() error { closes.Add(1); return nil }),
				Health:  client.health,
			}, nil
		}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())

		if rec := poolRequest(t, pool, 1); rec.Code != http.StatusOK {
			t.Fatalf("first request status = %d, want 200", rec.Code)
		}
		pool.mu.Lock()
		e := pool.entries[poolKey{id: 1}]
		if busy {
			e.inflight++
		}
		pool.mu.Unlock()
		clients[0].stop(errors.New("read tcp: connection reset by peer"))

		if rec := poolRequest(t, pool, 1); rec.Code != http.StatusOK {
			t.Errorf("busy=%v: status = %d, want 200 (rebuilt)", busy, rec.Code)
		}
		if got := builds.Load(); got != 2 {
			t.Errorf("busy=%v: builder ran %d times, want 2", busy, got)
		}
		if busy {
			if got := closes.Load(); got != 0 {
				t.Errorf("stopped assembly closed %d times while still held; want 0 (deferred to release)", got)
			}
			pool.release(e)
		}
		if got := closes.Load(); got != 1 {
			t.Errorf("busy=%v: stopped assembly closed %d times, want 1", busy, got)
		}
	}
}

// TestUserPoolRejectsAssemblyWithoutHealthProbe pins that a build missing its
// liveness probe fails instead of being served as "always healthy", and that
// what it did build is closed.
func TestUserPoolRejectsAssemblyWithoutHealthProbe(t *testing.T) {
	var closes atomic.Int64
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		return builtAssembly{Handler: okHandler(), Closer: closerFunc(func() error { closes.Add(1); return nil })}, nil
	}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())

	if rec := poolRequest(t, pool, 1); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if got := closes.Load(); got != 1 {
		t.Errorf("incomplete assembly closed %d times, want 1", got)
	}
}

// TestUserPoolEvictReleaseTransitionIsAtomic exercises both possible lock
// orders between eviction and the final release. The mutex-protected state
// machine must close exactly once without a production-only interposition
// hook or an atomic flag handshake.
func TestUserPoolEvictReleaseTransitionIsAtomic(t *testing.T) {
	for range 100 {
		var closes atomic.Int64
		pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
			return builtAssembly{}, errors.New("unused")
		}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())
		key := poolKey{id: 1}
		e := &userEntry{
			key: key, ready: make(chan struct{}), state: entryActive,
			handler: okHandler(), closer: closerFunc(func() error { closes.Add(1); return nil }),
			inflight: 1, lastUsed: time.Now(),
		}
		close(e.ready)
		pool.mu.Lock()
		pool.entries[key] = e
		pool.mu.Unlock()

		start := make(chan struct{})
		done := make(chan struct{}, 2)
		go func() { <-start; pool.EvictSession(1, ""); done <- struct{}{} }()
		go func() { <-start; pool.release(e); done <- struct{}{} }()
		close(start)
		<-done
		<-done
		if got := closes.Load(); got != 1 {
			t.Fatalf("close count = %d, want exactly 1", got)
		}
	}
}

// TestUserPoolBuilderPanic pins runBuild's recover path: a panicking builder
// must surface as a 503 to the requester, unblock a concurrent waiter with the
// same error, leave the pool empty (no cached poisoned entry), and let the next
// request trigger a fresh build.
func TestUserPoolBuilderPanic(t *testing.T) {
	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			panic("simulated builder panic")
		}
		return okAssembly(), nil
	}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())

	codes := make(chan int, 2)
	go func() { codes <- poolRequest(t, pool, 1).Code }()
	<-entered // the build is in flight; the entry is published as not-done
	go func() { codes <- poolRequest(t, pool, 1).Code }()

	// Wait until the second request has attached to the in-flight entry as a
	// waiter (creator + waiter = inflight 2) before releasing the panic.
	// Otherwise the builder can panic and delete the entry before the second
	// request finds it, so that request starts a fresh build and returns 200 —
	// a race that made this test flaky rather than a production bug.
	key := poolKeyFor(&authsrv.UserIdentity{ID: 1})
	waitFor(t, func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		e := pool.entries[key]
		return e != nil && e.inflight == 2
	})

	close(release) // let the builder panic

	for range 2 {
		if code := <-codes; code != http.StatusServiceUnavailable {
			t.Fatalf("request during a panicking build: status = %d, want 503", code)
		}
	}
	if pool.size() != 0 {
		t.Errorf("panicked build must not stay cached; size = %d, want 0", pool.size())
	}
	if rec := poolRequest(t, pool, 1); rec.Code != http.StatusOK {
		t.Errorf("request after a panicked build: status = %d, want 200 (fresh rebuild)", rec.Code)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("builder ran %d times, want 2 (panicked once, rebuilt once)", got)
	}
}

// TestUserPoolCloseDuringBuild pins the shutdown handoff for an in-flight
// build: Close() evicts the not-yet-done entry and returns without closing it;
// when the build completes and publishes a live Closer, the creator's release()
// observes the evicted state and tears it down exactly once.
func TestUserPoolCloseDuringBuild(t *testing.T) {
	var closes atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		close(entered)
		<-release
		return builtAssembly{
			Handler: okHandler(),
			Closer:  closerFunc(func() error { closes.Add(1); return nil }),
			Health:  healthy,
		}, nil
	}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())

	done := make(chan int, 1)
	go func() { done <- poolRequest(t, pool, 1).Code }()
	<-entered

	if err := pool.Close(); err != nil {
		t.Fatalf("Close during an in-flight build: %v", err)
	}
	if got := closes.Load(); got != 0 {
		t.Fatalf("Close must not tear down a still-building entry (closer not published yet); closes = %d", got)
	}

	close(release) // build completes AFTER shutdown snapshotted the map
	<-done
	if got := closes.Load(); got != 1 {
		t.Errorf("build completing after Close: closer ran %d times, want exactly 1 (0 = shutdown leak)", got)
	}
}

// TestUserPoolEvictSessionGraceForceClose pins the eviction grace bound: a
// built assembly still held by a hung in-flight request past
// userPoolEvictGrace is force-closed by the pool's single janitor timer, and
// the eventual release() does not double-close. It also pins that a
// still-building entry is only eligible for a deadline after its closer has
// been published.
func TestUserPoolEvictSessionGraceForceClose(t *testing.T) {
	t.Run("busy built entry force-closed after grace", func(t *testing.T) {
		var closes atomic.Int64
		pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
			return builtAssembly{
				Handler: okHandler(),
				Closer:  closerFunc(func() error { closes.Add(1); return nil }),
				Health:  healthy,
			}, nil
		}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())
		var skew atomic.Int64
		pool.now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
		janitorCtx, cancelJanitor := context.WithCancel(t.Context())
		defer cancelJanitor()
		go pool.janitor(janitorCtx)

		if rec := poolRequest(t, pool, 1); rec.Code != http.StatusOK {
			t.Fatalf("first request status = %d, want 200", rec.Code)
		}
		pool.mu.Lock()
		e := pool.entries[poolKey{id: 1}]
		e.inflight++ // a hung stream that never releases in time
		pool.mu.Unlock()

		pool.EvictSession(1, "")
		if got := closes.Load(); got != 0 {
			t.Fatalf("busy entry closed synchronously (%d); the close must wait for release or the grace timer", got)
		}
		skew.Store(int64(2 * userPoolEvictGrace))
		pool.signalJanitor()
		deadline := time.Now().Add(5 * time.Second)
		for closes.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if got := closes.Load(); got != 1 {
			t.Fatalf("grace timer did not force-close the held assembly; closes = %d, want 1", got)
		}
		// The hung holder finally releases: entryClosed absorbs the tie.
		pool.release(e)
		if got := closes.Load(); got != 1 {
			t.Errorf("release after the timer close double-closed; closes = %d, want 1", got)
		}
	})

	t.Run("building entry is closed by its creator, not a timer", func(t *testing.T) {
		var closes atomic.Int64
		entered := make(chan struct{})
		release := make(chan struct{})
		pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
			close(entered)
			<-release
			return builtAssembly{
				Handler: okHandler(),
				Closer:  closerFunc(func() error { closes.Add(1); return nil }),
				Health:  healthy,
			}, nil
		}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())
		var skew atomic.Int64
		pool.now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }

		done := make(chan int, 1)
		go func() { done <- poolRequest(t, pool, 1).Code }()
		<-entered

		pool.EvictSession(1, "") // the build has no closer to schedule yet
		// Push the clock well past the grace so an incorrectly scheduled
		// deadline would already be due before the closer exists.
		skew.Store(int64(2 * userPoolEvictGrace))
		close(release)
		<-done
		if got := closes.Load(); got != 1 {
			t.Errorf("creator's release must close the evicted build exactly once; closes = %d, want 1", got)
		}
	})
}

func TestUserPoolEvictIdleClosesAssembly(t *testing.T) {
	var closed atomic.Bool
	now := time.Now()
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		return builtAssembly{Handler: okHandler(), Closer: closerFunc(func() error { closed.Store(true); return nil }), Health: healthy}, nil
	}, unexpectedDrop(t), testWWWAuthenticate, discardLogger())
	pool.now = func() time.Time { return now }

	if rec := poolRequest(t, pool, 1); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	now = now.Add(userPoolIdleTTL + time.Minute)
	pool.evictIdle()
	if !closed.Load() {
		t.Error("idle assembly was not closed by evictIdle")
	}
	if pool.size() != 0 {
		t.Errorf("pool size after eviction = %d, want 0", pool.size())
	}
}
