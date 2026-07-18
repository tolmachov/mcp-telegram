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
	pool.entries[sessionKey{id: 1}].inflight.Add(1)
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
