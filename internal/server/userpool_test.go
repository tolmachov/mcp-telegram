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

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

const testWWWAuthenticate = `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"`

// poolRequest sends one request through the pool behind a stub bearer
// verifier, so the identity reaches the pool exactly the way the production
// RequireBearerToken middleware delivers it. The session id is empty (the
// legacy single-session case); use poolRequestSID for independent sessions.
func poolRequest(t *testing.T, p *userPool, id tgid.UserID) *httptest.ResponseRecorder {
	return poolRequestSID(t, p, id, "")
}

// poolRequestSID is poolRequest with an explicit per-authorization session id,
// so tests can drive several independent sessions of one account.
func poolRequestSID(t *testing.T, p *userPool, id tgid.UserID, sid string) *httptest.ResponseRecorder {
	t.Helper()
	verifier := func(_ context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
		return authsrv.NewTokenInfoForTesting(id, "u", sid, nil, time.Now().Add(time.Hour)), nil
	}
	handler := auth.RequireBearerToken(verifier, nil)(p)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
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
// session's assembly (even though it is idle-but-warm) so a revoked/upgraded
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
	e.inflight.Add(1)
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
	pool.entries[poolKey{id: 1}].inflight.Add(1)
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
	e.inflight.Add(1)
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

// TestUserPoolDeadBusyCloseNoLeakOnRaceOrder deterministically pins the fix for
// the dead-but-busy zero-close. The leak window is a single interleaving: the
// last holder's release() decrements inflight to 0 and reads the close flag as
// unset in the instant AFTER entryFor decides the entry is busy but BEFORE
// entryFor stores the flag. A probabilistic stress test can't reliably hit that
// window (the two ops sat a map-delete apart), so we use entryFor's test hook to
// interpose release() at exactly that point. Pre-fix, neither side closes and the
// map-detached entry leaks; post-fix, entryFor re-reads inflight after the store
// and closes it.
func TestUserPoolDeadBusyCloseNoLeakOnRaceOrder(t *testing.T) {
	var closes atomic.Int64
	pool := newUserPool(t.Context(), func(_ context.Context, _ *authsrv.UserIdentity) (builtAssembly, error) {
		return builtAssembly{}, errors.New("no rebuild in this test")
	}, testWWWAuthenticate, discardLogger())

	key := poolKey{id: 1}
	errDead := errors.New("client run loop exited")
	e := &userEntry{key: key, ready: make(chan struct{})}
	e.finish(builtAssembly{
		Handler: okHandler(),
		Closer:  closerFunc(func() error { closes.Add(1); return nil }),
		Health:  func() error { return errDead },
	})
	e.inflight.Store(1) // the hung in-flight holder
	pool.mu.Lock()
	pool.entries[key] = e
	pool.mu.Unlock()

	// The holder releases at the worst possible moment: after entryFor has
	// committed to the dead-but-busy branch, before it stores the close flag.
	// release() sees the flag unset and does not close, so the close must come
	// from entryFor's post-store recheck.
	pool.testHookDeadBusyBeforeStore = func() { pool.release(e) }

	if _, err := pool.entryFor(t.Context(), &authsrv.UserIdentity{ID: 1}); !errors.Is(err, tgclient.ErrSessionUnauthorized) {
		t.Fatalf("entryFor on a dead-but-busy entry: err = %v, want ErrSessionUnauthorized", err)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("dead entry closed %d times, want exactly 1 (0 = the leak this test pins, >1 = double close)", got)
	}
	if pool.size() != 0 {
		t.Errorf("entry must be removed from the map; size = %d, want 0", pool.size())
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
// build: Close() flags the not-yet-done entry and returns without closing it;
// when the build completes and publishes a live Closer, the creator's release()
// observes the flag and tears it down — exactly once (the closeOnce tie between
// that release and any other teardown path must not double-close).
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
// force-closed by the timer (bounding the post-upgrade auth-key overlap), and
// the eventual release() does not double-close. It also pins the guard that a
// still-BUILDING entry never gets a timer close: firing closeEntry before the
// closer is published would burn closeOnce with a nil closer and leak the real
// one forever.
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

		if rec := poolRequest(t, pool, 1); rec.Code != http.StatusOK {
			t.Fatalf("first request status = %d, want 200", rec.Code)
		}
		pool.mu.Lock()
		e := pool.entries[poolKey{id: 1}]
		e.inflight.Add(1) // a hung stream that never releases in time
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
		// The hung holder finally releases: closeOnce absorbs the tie.
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

		pool.EvictSession(1, "") // entry is not done: no timer may be armed
		// Give a wrongly-armed timer ample time to fire on the nil closer and
		// burn closeOnce — the pinned bug would then make closes stay 0 forever.
		time.Sleep(100 * time.Millisecond)
		close(release)
		<-done
		if got := closes.Load(); got != 1 {
			t.Errorf("creator's release must close the flagged build exactly once; closes = %d, want 1 (0 = closeOnce burned by a premature timer)", got)
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
