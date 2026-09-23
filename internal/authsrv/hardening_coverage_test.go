package authsrv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore/sessionstoretest"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

type panicAbortFlow struct{ *fakeFlow }

func (panicAbortFlow) Abort() { panic("abort panic") }

type noUserFlow struct{ *fakeFlow }

func (noUserFlow) User() (LoginUser, bool) { return LoginUser{}, false }

type failedSessionStore struct{ sessionstore.Store }

func (s failedSessionStore) Session(tgid.UserID, string, []byte) session.Storage {
	return failedSessionStorage{}
}

type failedSessionStorage struct{}

func (failedSessionStorage) LoadSession(context.Context) ([]byte, error) {
	return nil, session.ErrNotFound
}

func (failedSessionStorage) StoreSession(context.Context, []byte) error {
	return errors.New("simulated session write failure")
}

func TestAuthorizeBeforeStartFailsWithoutLaunchingTelegram(t *testing.T) {
	starts := 0
	a, err := New(testConfig(t), slog.New(slog.DiscardHandler), sessionstoretest.NewMemory(), func(context.Context) (LoginFlow, error) {
		starts++
		return newFakeFlow(), nil
	}, noInvalidate)
	require.NoError(t, err)
	t.Cleanup(a.Close)
	mux := http.NewServeMux()
	a.Routes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	clientID := registerClient(t, ts, testRedirectURI)
	_, challenge := pkcePair()

	resp, err := noRedirectClient().Get(ts.URL + "/authorize?" + url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {testRedirectURI},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode())
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	location, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "temporarily_unavailable", location.Query().Get("error"))
	assert.Zero(t, starts)
}

func TestAuthLifecycleAndPanicIsolationBranches(t *testing.T) {
	a, err := New(testConfig(t), slog.New(slog.DiscardHandler), sessionstoretest.NewMemory(), neverStartLogin, noInvalidate)
	require.NoError(t, err)
	assert.Error(t, a.Start(nil)) //nolint:staticcheck // Explicitly verifies the public nil-context rejection.
	require.NoError(t, a.Start(t.Context()))
	require.NoError(t, a.Start(t.Context()))
	a.Close()
	a.Close()
	assert.Error(t, a.Start(t.Context()))

	// A background-tick panic is isolated from its loop.
	a.runTick("test", func() { panic("tick panic") })

	// A malicious LoginFlow.Abort cannot unwind the pending-login sweeper.
	b, err := New(testConfig(t), slog.New(slog.DiscardHandler), sessionstoretest.NewMemory(), neverStartLogin, noInvalidate)
	require.NoError(t, err)
	flow := panicAbortFlow{newFakeFlow()}
	require.NoError(t, b.addPending("request", flow, "127.0.0.1"))
	b.now = func() time.Time { return time.Now().Add(pendingLoginTTL + time.Minute) }
	b.runTick("test", b.sweepExpiredPending)
	b.Close()
}

func TestTokenAndIdentityDefensiveBranches(t *testing.T) {
	a, ts := newTestServer(t, testConfig(t), sessionstoretest.NewMemory(), neverStartLogin)

	recorder := httptest.NewRecorder()
	a.mintTokens(recorder, mintInput{})
	assert.Equal(t, http.StatusInternalServerError, recorder.Code)

	resp, err := http.Post(ts.URL+"/token", "application/x-www-form-urlencoded", strings.NewReader(strings.Repeat("x", maxFormBody+1)))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	_, ok := IdentityFromTokenInfo(nil)
	assert.False(t, ok)
	_, ok = IdentityFromTokenInfo(&auth.TokenInfo{})
	assert.False(t, ok)
	_, ok = IdentityFromTokenInfo(&auth.TokenInfo{Extra: map[string]any{extraIdentityKey: "wrong type"}})
	assert.False(t, ok)
}

func TestLoginQRWithoutExportedTokenIsNotFound(t *testing.T) {
	a, _ := newTestServer(t, testConfig(t), sessionstoretest.NewMemory(), neverStartLogin)
	flow := newFakeFlow()
	flow.setURL("")
	require.NoError(t, a.addPending("request", flow, "127.0.0.1"))

	a.pendingMu.Lock()
	var id string
	for id = range a.pending {
		break
	}
	a.pendingMu.Unlock()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/login/qr?login="+id, nil)
	a.handleLoginQR(recorder, request)
	assert.Equal(t, http.StatusNotFound, recorder.Code)
}

func TestWriteJSONHandlesUnwritableResponse(t *testing.T) {
	a, _ := newTestServer(t, testConfig(t), sessionstoretest.NewMemory(), neverStartLogin)
	writer := &errorResponseWriter{header: make(http.Header)}
	a.writeJSON(writer, http.StatusOK, map[string]string{"ok": "yes"})
}

func TestFinalizeLoginRejectsIncompleteOrInvalidFlows(t *testing.T) {
	tests := []struct {
		name  string
		store sessionstore.Store
		flow  LoginFlow
		state string
	}{
		{
			name:  "invalid sealed authorization request",
			store: sessionstoretest.NewMemory(),
			flow: func() LoginFlow {
				f := newFakeFlow()
				f.complete(LoginUser{ID: allowedUser}, []byte("session"))
				return f
			}(),
			state: "corrupt",
		},
		{
			name:  "completed without user",
			store: sessionstoretest.NewMemory(),
			flow: func() LoginFlow {
				f := newFakeFlow()
				f.complete(LoginUser{ID: allowedUser}, []byte("session"))
				return noUserFlow{f}
			}(),
		},
		{
			name:  "user denied by allowlist",
			store: sessionstoretest.NewMemory(),
			flow: func() LoginFlow {
				f := newFakeFlow()
				f.complete(LoginUser{ID: forbiddenUser}, []byte("session"))
				return f
			}(),
		},
		{
			name:  "completed without session",
			store: sessionstoretest.NewMemory(),
			flow: func() LoginFlow {
				f := newFakeFlow()
				f.complete(LoginUser{ID: allowedUser}, nil)
				return f
			}(),
		},
		{
			name:  "session backend failure",
			store: failedSessionStore{Store: sessionstoretest.NewMemory()},
			flow: func() LoginFlow {
				f := newFakeFlow()
				f.complete(LoginUser{ID: allowedUser}, []byte("session"))
				return f
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := New(testConfig(t), slog.New(slog.DiscardHandler), tt.store, neverStartLogin, noInvalidate)
			require.NoError(t, err)
			t.Cleanup(a.Close)
			state := tt.state
			if state == "" {
				state, err = sealBlob(a.sealer, stateBlob, stateClaims{
					ClientID: "client", RedirectURI: testRedirectURI,
					CodeChallenge: "challenge", Resource: a.cfg.IssuerURL,
					IssuedAt: a.now().Unix(),
				})
				require.NoError(t, err)
			}
			p := &pendingLogin{id: "pending", request: state, flow: tt.flow, created: a.now()}
			a.pending[p.id] = p
			recorder := httptest.NewRecorder()
			a.finalizeLogin(t.Context(), recorder, p)
			assert.Equal(t, http.StatusOK, recorder.Code)
			var result pollResponse
			require.NoError(t, json.NewDecoder(recorder.Body).Decode(&result))
			assert.Equal(t, "failed", result.Status)
			_, exists := a.lookupPending(p.id)
			assert.False(t, exists)
		})
	}
}

func TestFinalizeLoginCleansSessionWhenRedirectCannotBeBuilt(t *testing.T) {
	store := sessionstoretest.NewMemory()
	a, err := New(testConfig(t), slog.New(slog.DiscardHandler), store, neverStartLogin, noInvalidate)
	require.NoError(t, err)
	t.Cleanup(a.Close)
	state, err := sealBlob(a.sealer, stateBlob, stateClaims{
		ClientID: "client", RedirectURI: "%",
		CodeChallenge: "challenge", Resource: a.cfg.IssuerURL,
		IssuedAt: a.now().Unix(),
	})
	require.NoError(t, err)
	flow := newFakeFlow()
	flow.complete(LoginUser{ID: allowedUser}, []byte("session"))
	p := &pendingLogin{id: "pending", request: state, flow: flow, created: a.now()}
	a.pending[p.id] = p

	recorder := httptest.NewRecorder()
	a.finalizeLogin(t.Context(), recorder, p)
	var result pollResponse
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&result))
	assert.Equal(t, "failed", result.Status)
	sessions, err := store.List(t.Context())
	require.NoError(t, err)
	assert.Empty(t, sessions, "uncommitted Telegram session must be removed")
}

func TestPasswordEndpointDefensiveBranches(t *testing.T) {
	a, _ := newTestServer(t, testConfig(t), sessionstoretest.NewMemory(), neverStartLogin)

	tooLarge := httptest.NewRequest(http.MethodPost, "/login/password", strings.NewReader(strings.Repeat("x", maxFormBody+1)))
	tooLarge.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	a.handleLoginPassword(recorder, tooLarge)
	assert.Equal(t, http.StatusBadRequest, recorder.Code)

	flow := newFakeFlow()
	require.NoError(t, a.addPending("request", flow, "127.0.0.1"))
	a.pendingMu.Lock()
	var id string
	for id = range a.pending {
		break
	}
	a.pendingMu.Unlock()
	recorder = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login/password", strings.NewReader(url.Values{
		"login": {id}, "password": {"ignored"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	a.handleLoginPassword(recorder, req)
	assert.Equal(t, http.StatusNoContent, recorder.Code)
}

func TestRegistrationRejectsInvalidMetadata(t *testing.T) {
	_, ts := newTestServer(t, testConfig(t), sessionstoretest.NewMemory(), neverStartLogin)
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: "{"},
		{name: "missing redirect URI", body: `{}`},
		{name: "unsupported grant", body: `{"redirect_uris":["http://localhost/callback"],"grant_types":["password"]}`},
		{name: "unsupported response", body: `{"redirect_uris":["http://localhost/callback"],"response_types":["token"]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := http.Post(ts.URL+"/register", "application/json", strings.NewReader(tt.body))
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
	assert.Equal(t, "An MCP client", clientDisplayName(&clientIDClaims{}))
}

func TestAuthorizeRejectsProtocolErrorsAndLoginStartFailure(t *testing.T) {
	a, err := New(testConfig(t), slog.New(slog.DiscardHandler), sessionstoretest.NewMemory(), func(context.Context) (LoginFlow, error) {
		return nil, errors.New("simulated Telegram startup failure")
	}, noInvalidate)
	require.NoError(t, err)
	t.Cleanup(a.Close)
	require.NoError(t, a.Start(t.Context()))
	a.limiter = newIPRateLimiter(100000, 100000, 0)
	mux := http.NewServeMux()
	a.Routes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	clientID := registerClient(t, ts, testRedirectURI)
	_, challenge := pkcePair()

	base := url.Values{
		"client_id": {clientID}, "redirect_uri": {testRedirectURI},
		"response_type": {"code"}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "state": {"roundtrip"},
	}
	tests := []struct {
		name string
		edit func(url.Values)
		want string
	}{
		{name: "unsupported response", edit: func(q url.Values) { q.Set("response_type", "token") }, want: "unsupported_response_type"},
		{name: "missing PKCE", edit: func(q url.Values) { q.Del("code_challenge") }, want: "invalid_request"},
		{name: "foreign resource", edit: func(q url.Values) { q.Set("resource", "https://other.example") }, want: "invalid_target"},
		{name: "login startup", edit: func(url.Values) {}, want: "server_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := url.Values{}
			for key, values := range base {
				q[key] = append([]string(nil), values...)
			}
			tt.edit(q)
			resp, err := noRedirectClient().Get(ts.URL + "/authorize?" + q.Encode())
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, http.StatusFound, resp.StatusCode)
			location, err := url.Parse(resp.Header.Get("Location"))
			require.NoError(t, err)
			assert.Equal(t, tt.want, location.Query().Get("error"))
			assert.Equal(t, "roundtrip", location.Query().Get("state"))
		})
	}
}

func TestInvalidAuthorizeClientAndRedirectAreNeverFollowed(t *testing.T) {
	_, ts := newTestServer(t, testConfig(t), sessionstoretest.NewMemory(), neverStartLogin)
	resp, err := http.Get(ts.URL + "/authorize?client_id=invalid&redirect_uri=https://attacker.example")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	clientID := registerClient(t, ts, testRedirectURI)
	resp, err = http.Get(ts.URL + "/authorize?" + url.Values{
		"client_id": {clientID}, "redirect_uri": {"https://attacker.example"},
	}.Encode())
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	recorder := httptest.NewRecorder()
	redirectError(recorder, httptest.NewRequest(http.MethodGet, "/", nil), "%", "", "invalid_request", "bad")
	assert.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestVerifierRejectsMalformedSubjectAndForeignResource(t *testing.T) {
	a, _ := newTestServer(t, testConfig(t), sessionstoretest.NewMemory(), neverStartLogin)
	now := a.now()
	base := accessClaims{
		Subject: allowedUser.String(), ClientID: "client", Resource: a.cfg.IssuerURL,
		SessionID:  "0123456789abcdef0123456789abcdef",
		SessionKey: make([]byte, sessionKeyLen),
		Family:     "fedcba9876543210fedcba9876543210",
		IssuedAt:   now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	tests := []struct {
		name   string
		mutate func(*accessClaims)
	}{
		{name: "malformed subject", mutate: func(c *accessClaims) { c.Subject = "not-a-user" }},
		{name: "foreign resource", mutate: func(c *accessClaims) { c.Resource = "https://other.example" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := base
			tt.mutate(&claims)
			token, err := sealBlob(a.sealer, accessBlob, claims)
			require.NoError(t, err)
			_, err = a.Verifier()(t.Context(), token, nil)
			assert.ErrorIs(t, err, auth.ErrInvalidToken)
		})
	}
}

func TestLoginHandlersCoverConcurrentAndImpossibleStates(t *testing.T) {
	a, _ := newTestServer(t, testConfig(t), sessionstoretest.NewMemory(), neverStartLogin)

	qrFlow := newFakeFlow()
	require.NoError(t, a.addPending("request", qrFlow, "127.0.0.1"))
	a.pendingMu.Lock()
	var qrID string
	for qrID = range a.pending {
		break
	}
	a.pendingMu.Unlock()
	a.handleLoginQR(&errorResponseWriter{header: make(http.Header)}, httptest.NewRequest(http.MethodGet, "/login/qr?login="+qrID, nil))
	a.removePending(qrID)

	consumed := newFakeFlow()
	consumed.complete(LoginUser{ID: allowedUser}, []byte("session"))
	p := &pendingLogin{id: "consumed", request: "request", flow: consumed, created: a.now(), consumed: true}
	a.pending[p.id] = p
	recorder := httptest.NewRecorder()
	a.handleLoginPoll(recorder, httptest.NewRequest(http.MethodGet, "/login/poll?login="+p.id, nil))
	var result pollResponse
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&result))
	assert.Equal(t, "waiting", result.Status)
	a.removePending(p.id)

	impossible := newFakeFlow()
	impossible.mu.Lock()
	impossible.state = LoginState(99)
	impossible.mu.Unlock()
	p = &pendingLogin{id: "impossible", request: "request", flow: impossible, created: a.now()}
	a.pending[p.id] = p
	recorder = httptest.NewRecorder()
	a.handleLoginPoll(recorder, httptest.NewRequest(http.MethodGet, "/login/poll?login="+p.id, nil))
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&result))
	assert.Equal(t, "failed", result.Status)
}

type errorResponseWriter struct{ header http.Header }

func (w *errorResponseWriter) Header() http.Header       { return w.header }
func (w *errorResponseWriter) WriteHeader(int)           {}
func (w *errorResponseWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
