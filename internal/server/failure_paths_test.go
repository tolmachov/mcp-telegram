package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/tgerr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore/sessionstoretest"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

type assemblyLoadFailure struct {
	sessionstore.Store
	loadErr   error
	deleted   []string
	deleteErr error
}

func (s *assemblyLoadFailure) Session(tgid.UserID, string, []byte) session.Storage {
	return failingAssemblySession{err: s.loadErr}
}

func (s *assemblyLoadFailure) Delete(_ context.Context, _ tgid.UserID, sid string) error {
	s.deleted = append(s.deleted, sid)
	return s.deleteErr
}

type failingAssemblySession struct{ err error }

func (s failingAssemblySession) LoadSession(context.Context) ([]byte, error) { return nil, s.err }
func (s failingAssemblySession) StoreSession(context.Context, []byte) error  { return s.err }

func TestAssemblyStartupPreservesUnavailableSessions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		loadErr    error
		deleteErr  error
		wantDelete bool
	}{
		{name: "corrupt ciphertext is preserved", loadErr: sessionstore.ErrCorruptSession},
		{name: "access denied is preserved", loadErr: errors.New("session store unavailable")},
		{name: "unauthorized session is removed", loadErr: tgclient.ErrSessionUnauthorized, wantDelete: true},
		{name: "delete failure preserves unauthorized error", loadErr: tgclient.ErrSessionUnauthorized, deleteErr: errors.New("delete failed"), wantDelete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &assemblyLoadFailure{Store: sessionstoretest.NewMemory(), loadErr: tc.loadErr, deleteErr: tc.deleteErr}
			s := testServer(t)
			s.opts.Config = &tgclient.Config{APIID: 1, APIHash: "test"}
			s.opts.SessionStore = store
			pool := newUserPool(t.Context(), s.userAssemblyBuilder(), store.Delete, "Bearer", s.logger)
			t.Cleanup(func() { _ = pool.Close() })
			rec := httptest.NewRecorder()
			pool.serveUser(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil), &authsrv.UserIdentity{ID: 42, SessionID: "target"})
			if tc.wantDelete {
				assert.Equal(t, http.StatusUnauthorized, rec.Code, "a failed delete must not hide the refusal")
				assert.Equal(t, []string{"target"}, store.deleted)
			} else {
				assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
				assert.Empty(t, store.deleted)
			}
		})
	}
}

func TestAssemblyStartupClassifiesTelegramErrors(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		unauthorized bool
	}{
		{"unregistered key", tgerr.New(401, "AUTH_KEY_UNREGISTERED"), true},
		{"expired session", tgerr.New(406, "SESSION_EXPIRED"), true},
		{"duplicated key", tgerr.New(406, "AUTH_KEY_DUPLICATED"), true},
		{"revoked session", tgerr.New(401, "SESSION_REVOKED"), true},
		{"storage access denied", errors.New("access denied"), false},
		{"corrupt storage", sessionstore.ErrCorruptSession, false},
		{"transport timeout", context.DeadlineExceeded, false},
		{"flood wait", tgerr.New(420, "FLOOD_WAIT_30"), false},
		{"telegram unavailable", tgerr.New(500, "INTERNAL_SERVER_ERROR"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Inject at the startup boundary without contacting Telegram. Unlike
			// the sentinel-only tests, these errors have gotd's native types.
			store := &assemblyLoadFailure{Store: sessionstoretest.NewMemory(), loadErr: fmt.Errorf("startup: %w", tc.err)}
			s := testServer(t)
			s.opts.Config = &tgclient.Config{APIID: 1, APIHash: "test"}
			s.opts.SessionStore = store
			pool := newUserPool(t.Context(), s.userAssemblyBuilder(), store.Delete, "Bearer", s.logger)
			t.Cleanup(func() { _ = pool.Close() })
			rec := httptest.NewRecorder()
			pool.serveUser(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil), &authsrv.UserIdentity{ID: 42, SessionID: "target"})
			if tc.unauthorized {
				assert.Equal(t, http.StatusUnauthorized, rec.Code)
				assert.Equal(t, "Bearer", rec.Header().Get("WWW-Authenticate"))
				assert.Equal(t, []string{"target"}, store.deleted)
			} else {
				assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
				assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
				assert.Empty(t, store.deleted)
			}
		})
	}
}

func TestRequestCapacityIsReleasedAfterHandlersExit(t *testing.T) {
	entered := make(chan struct{}, httpMaxConcurrentRequests)
	finished := make(chan struct{}, httpMaxConcurrentRequests)
	release := make(chan struct{})
	handler := withRequestLimits(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	var wg sync.WaitGroup
	for range httpMaxConcurrentRequests {
		wg.Go(func() {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
			finished <- struct{}{}
		})
	}
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() { unblock(); wg.Wait() })
	for range httpMaxConcurrentRequests {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("handlers did not start")
		}
	}
	overflow := httptest.NewRecorder()
	handler.ServeHTTP(overflow, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusServiceUnavailable, overflow.Code)
	// Let one request finish; the next request must be admitted again.
	release <- struct{}{}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish")
	}
	admitted := make(chan int, 1)
	wg.Go(func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		admitted <- rec.Code
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("capacity was not released")
	}
	unblock()
	select {
	case status := <-admitted:
		assert.Equal(t, http.StatusNoContent, status)
	case <-time.After(5 * time.Second):
		t.Fatal("admitted request did not finish")
	}
}

func TestServeHTTPReportsBindFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close() //nolint:errcheck // test cleanup
	err = testServer(t).serveHTTP(t.Context(), http.NotFoundHandler(), listener.Addr().String())
	require.ErrorContains(t, err, "http server")
}

func TestServeHTTPForcesOpenStreamClosedOnShutdown(t *testing.T) {
	addr := freePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		<-r.Context().Done()
	})
	go func() { done <- testServer(t).serveHTTP(ctx, handler, addr) }()
	response := waitForServer(t, "http://"+addr)
	defer response.Body.Close() //nolint:errcheck // test cleanup
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("open stream prevented shutdown")
	}
	_, err := io.ReadAll(response.Body)
	assert.Error(t, err, "force-closing the stream interrupts the response body")
}
