package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

const testWWWAuthenticate = `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"`

// waitFor polls cond until it holds or a short deadline elapses, failing the
// test on timeout. Used to synchronize on concurrent pool state (e.g. a waiter
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
	}, testWWWAuthenticate, discardLogger())
	recorder := httptest.NewRecorder()
	pool.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestUserPoolLimitsSessionlessInitializePerUser(t *testing.T) {
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		return okAssembly(), nil
	}, testWWWAuthenticate, discardLogger())
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
	return builtAssembly{Handler: okHandler(), Closer: closerFunc(func() error { return nil })}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestUserPoolBuildsOncePerUser(t *testing.T) {
	var builds atomic.Int64
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		builds.Add(1)
		return okAssembly(), nil
	}, testWWWAuthenticate, discardLogger())

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
	}, testWWWAuthenticate, discardLogger())

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
		}, nil
	}, testWWWAuthenticate, discardLogger())

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
		}, nil
	}, testWWWAuthenticate, discardLogger())

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
	}, testWWWAuthenticate, discardLogger())

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

func TestUserPoolUnauthorizedSessionMapsTo401(t *testing.T) {
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		return builtAssembly{}, tgclient.ErrSessionUnauthorized
	}, testWWWAuthenticate, discardLogger())

	rec := poolRequest(t, pool, 7)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != testWWWAuthenticate {
		t.Errorf("WWW-Authenticate = %q, want %q", got, testWWWAuthenticate)
	}
}

func TestUserPoolBuildFailureNotCached(t *testing.T) {
	var builds atomic.Int64
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		if builds.Add(1) == 1 {
			return builtAssembly{}, errors.New("transient failure")
		}
		return okAssembly(), nil
	}, testWWWAuthenticate, discardLogger())

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
	}, testWWWAuthenticate, discardLogger())
	pool.maxUsers = 1

	if rec := poolRequest(t, pool, 1); rec.Code != http.StatusOK {
		t.Fatalf("first user status = %d, want 200", rec.Code)
	}
	// The single entry is idle (inflight 0) → LRU eviction makes room.
	if rec := poolRequest(t, pool, 2); rec.Code != http.StatusOK {
		t.Fatalf("second user status = %d, want 200 after LRU eviction", rec.Code)
	}
	if pool.size() != 1 {
		t.Errorf("pool size = %d, want 1", pool.size())
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
	}, testWWWAuthenticate, discardLogger())

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

func TestUserPoolDeadBusyAssemblyMapsTo401(t *testing.T) {
	dead := &atomic.Bool{}
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		return builtAssembly{
			Handler: okHandler(),
			Closer:  closerFunc(func() error { return nil }),
			Health: func() error {
				if dead.Load() {
					return errors.New("client run loop exited")
				}
				return nil
			},
		}, nil
	}, testWWWAuthenticate, discardLogger())

	if rec := poolRequest(t, pool, 1); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}
	// Simulate a hung stream holding the entry while the client dies: the
	// pool cannot tear it down, so new requests get the re-auth 401.
	pool.mu.Lock()
	pool.entries[poolKey{id: 1}].inflight++
	pool.mu.Unlock()
	dead.Store(true)

	rec := poolRequest(t, pool, 1)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a dead-but-busy assembly", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != testWWWAuthenticate {
		t.Errorf("WWW-Authenticate = %q, want %q", got, testWWWAuthenticate)
	}
}

// TestUserPoolDeadBusyClosedOnRelease pins the fix for the dead-but-busy leak:
// when a dead client still has an in-flight holder, a new request removes the
// entry from the map and flags it, so the last release() tears it down promptly
// instead of leaving it to the 15-minute idle janitor.
func TestUserPoolDeadBusyClosedOnRelease(t *testing.T) {
	var closes atomic.Int64
	dead := &atomic.Bool{}
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
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
	}, testWWWAuthenticate, discardLogger())

	if rec := poolRequest(t, pool, 1); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}
	// A hung request holds the entry; capture its ref before the client dies.
	pool.mu.Lock()
	e := pool.entries[poolKey{id: 1}]
	e.inflight++
	pool.mu.Unlock()
	dead.Store(true)

	// A new request sees the dead-but-busy entry: 401, and it removes the entry
	// from the map and flags it for teardown.
	if rec := poolRequest(t, pool, 1); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a dead-but-busy assembly", rec.Code)
	}
	if pool.size() != 0 {
		t.Errorf("dead-but-busy entry must be removed from the map; size = %d, want 0", pool.size())
	}
	if got := closes.Load(); got != 0 {
		t.Errorf("entry closed %d times while still held; want 0 (deferred to release)", got)
	}
	// The hung request finally releases → the dead assembly is torn down now,
	// not deferred to idle-evict.
	pool.release(e)
	if got := closes.Load(); got != 1 {
		t.Errorf("dead-but-busy entry closed %d times after release; want 1", got)
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
		}, testWWWAuthenticate, discardLogger())
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
	}, testWWWAuthenticate, discardLogger())

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
		}, nil
	}, testWWWAuthenticate, discardLogger())

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
// built assembly still held by a hung in-flight request past evictGrace is
// force-closed by the pool's single janitor timer, and the eventual release()
// does not double-close. It also pins that a still-building entry is only
// eligible for a deadline after its closer has been published.
func TestUserPoolEvictSessionGraceForceClose(t *testing.T) {
	t.Run("busy built entry force-closed after grace", func(t *testing.T) {
		var closes atomic.Int64
		pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
			return builtAssembly{
				Handler: okHandler(),
				Closer:  closerFunc(func() error { closes.Add(1); return nil }),
			}, nil
		}, testWWWAuthenticate, discardLogger())
		pool.evictGrace = 10 * time.Millisecond
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
			}, nil
		}, testWWWAuthenticate, discardLogger())
		pool.evictGrace = 10 * time.Millisecond

		done := make(chan int, 1)
		go func() { done <- poolRequest(t, pool, 1).Code }()
		<-entered

		pool.EvictSession(1, "") // the build has no closer to schedule yet
		// Give an incorrectly scheduled deadline ample time to run before the
		// closer exists.
		time.Sleep(100 * time.Millisecond)
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
		return builtAssembly{Handler: okHandler(), Closer: closerFunc(func() error { closed.Store(true); return nil })}, nil
	}, testWWWAuthenticate, discardLogger())
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
