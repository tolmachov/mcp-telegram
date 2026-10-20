package authsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore/sessionstoretest"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// gatedLoginStore pauses the real session write so other polls can arrive
// while the successful poll owns the result, including after its request is gone.
type gatedLoginStore struct {
	sessionstore.Store
	entered chan struct{}
	release chan struct{}
}

func (s *gatedLoginStore) Session(id tgid.UserID, sid string, key []byte) session.Storage {
	return gatedLoginSession{Storage: s.Store.Session(id, sid, key), entered: s.entered, release: s.release}
}

type gatedLoginSession struct {
	session.Storage
	entered chan<- struct{}
	release <-chan struct{}
}

func (s gatedLoginSession) StoreSession(ctx context.Context, data []byte) error {
	select {
	case s.entered <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-s.release:
		if err := s.Storage.StoreSession(ctx, data); err != nil {
			return fmt.Errorf("storing login session: %w", err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSuccessfulLoginFinalisesOnceAfterPollCancellation(t *testing.T) {
	store := &gatedLoginStore{Store: sessionstoretest.New(t), entered: make(chan struct{}, 16), release: make(chan struct{})}
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(store.release) }) })
	flow := newFakeFlow()
	a, ts := newTestServer(t, testConfig(t), store, startOne(flow))
	clientID := registerClient(t, ts, testRedirectURI)
	_, challenge := pkcePair()
	loginID := startAuthorize(t, ts, clientID, challenge, "state")
	flow.complete(LoginUser{ID: allowedUser}, []byte("final session"))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	recorder := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		a.handleLoginPoll(recorder, httptest.NewRequestWithContext(ctx, http.MethodGet, "/login/poll?login="+loginID, nil))
	}()
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("successful poll did not start persisting its session")
	}
	cancel()

	const followers = 8
	replies := make(chan *httptest.ResponseRecorder, followers)
	for range followers {
		go func() {
			other := httptest.NewRecorder()
			a.handleLoginPoll(other, httptest.NewRequest(http.MethodGet, "/login/poll?login="+loginID, nil))
			replies <- other
		}()
	}
	for range followers {
		select {
		case reply := <-replies:
			var response pollResponse
			require.NoError(t, json.NewDecoder(reply.Body).Decode(&response))
			assert.Equal(t, "waiting", response.Status)
		case <-time.After(5 * time.Second):
			t.Fatal("another poll attempted to commit an already claimed login")
		}
	}
	release.Do(func() { close(store.release) })
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("login did not finish after storage resumed")
	}
	var response pollResponse
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&response))
	require.Equal(t, "done", response.Status)
	redirect, err := url.Parse(response.Redirect)
	require.NoError(t, err)
	code, err := openBlob(a.sealer, codeBlob, redirect.Query().Get("code"), a.now())
	require.NoError(t, err)
	stored, err := store.Session(code.Subject, code.SessionID, code.SessionKey).LoadSession(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []byte("final session"), stored)
	refs, err := store.List(t.Context())
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, code.SessionID, refs[0].SID)
	assert.Equal(t, "expired", pollLogin(t, ts, loginID).Status)
}
