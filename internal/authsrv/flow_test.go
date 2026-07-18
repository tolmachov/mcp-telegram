package authsrv

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

const (
	testIssuer                  = "https://issuer.example"
	testRedirectURI             = "http://localhost:41234/callback"
	allowedUser     tgid.UserID = 123456
	forbiddenUser   tgid.UserID = 999999
)

// fakeFlow is a scriptable LoginFlow driven by the tests.
type fakeFlow struct {
	mu        sync.Mutex
	url       string
	state     LoginState
	user      LoginUser
	session   []byte
	err       error
	done      chan struct{}
	doneOnce  sync.Once
	aborted   bool
	passwords []string // every submitted password, in order
	pwOK      string   // password that completes the login (when non-empty)
	pwUser    LoginUser
	pwSession []byte
}

func newFakeFlow() *fakeFlow {
	return &fakeFlow{url: "tg://login?token=initial", done: make(chan struct{})}
}

func (f *fakeFlow) TokenURL() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.url, f.url != ""
}

func (f *fakeFlow) State() LoginState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeFlow) User() (LoginUser, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.user, f.state == LoginDone
}

func (f *fakeFlow) SessionData() ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.session, f.state == LoginDone
}

func (f *fakeFlow) SubmitPassword(pw string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != LoginPasswordNeeded {
		return false
	}
	f.passwords = append(f.passwords, pw)
	if f.pwOK != "" && pw == f.pwOK {
		f.user, f.session, f.err = f.pwUser, f.pwSession, nil
		f.state = LoginDone
		f.doneOnce.Do(func() { close(f.done) })
	} else {
		f.err = errors.New("PASSWORD_HASH_INVALID")
	}
	return true
}

func (f *fakeFlow) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeFlow) Done() <-chan struct{} { return f.done }

func (f *fakeFlow) Abort() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted = true
	if f.state != LoginDone && f.state != LoginFailed {
		f.state = LoginFailed
		f.err = context.Canceled
	}
	f.doneOnce.Do(func() { close(f.done) })
}

func (f *fakeFlow) wasAborted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.aborted
}

// setURL simulates Telegram rotating the login token.
func (f *fakeFlow) setURL(u string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.url = u
}

// complete simulates a confirmed QR scan: user/session and the LoginDone
// state are published before Done is closed, so a consumer that receives on
// Done() then reads User()/SessionData() always sees final values — the
// LoginFlow contract the real QRFlow guarantees via its teardown ordering.
func (f *fakeFlow) complete(user LoginUser, session []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.user, f.session = user, session
	f.state = LoginDone
	f.doneOnce.Do(func() { close(f.done) })
}

// needPassword moves the flow into the 2FA phase; password acceptance is
// scripted via pwOK.
func (f *fakeFlow) needPassword(okPassword string, user LoginUser, session []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = LoginPasswordNeeded
	f.pwOK, f.pwUser, f.pwSession = okPassword, user, session
}

// failNow simulates a terminal login failure.
func (f *fakeFlow) failNow(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = LoginFailed
	f.err = err
	f.doneOnce.Do(func() { close(f.done) })
}

// startOne returns a StartLoginFunc handing out the given flows in order and
// failing when more logins are started than scripted.
func startOne(flows ...*fakeFlow) StartLoginFunc {
	var mu sync.Mutex
	i := 0
	return func(context.Context) (LoginFlow, error) {
		mu.Lock()
		defer mu.Unlock()
		if i >= len(flows) {
			return nil, fmt.Errorf("unexpected login start #%d", i+1)
		}
		f := flows[i]
		i++
		return f, nil
	}
}

// neverStartLogin fails the test if any login flow is started.
func neverStartLogin(context.Context) (LoginFlow, error) {
	return nil, fmt.Errorf("no login expected in this test")
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{
		IssuerURL: testIssuer,
		Allow:     AllowUsers(allowedUser),
		TokenKeys: []string{testKey(t)},
	}
}

// newTestServer wires an AuthServer with the given store and login starter
// and returns an httptest server with the auth routes mounted. The rate
// limiter is made effectively unlimited so protocol tests are not coupled to
// the budget.
func newTestServer(t *testing.T, cfg *Config, store sessionstore.Store, start StartLoginFunc) (*AuthServer, *httptest.Server) {
	t.Helper()
	a, err := New(cfg, slog.New(slog.DiscardHandler), store, start)
	require.NoError(t, err)
	t.Cleanup(a.Close)
	a.limiter = newIPRateLimiter(100000, 100000)

	mux := http.NewServeMux()
	a.Routes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return a, ts
}

// noRedirectClient never follows redirects: the tests inspect Location.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func pkcePair() (verifier, challenge string) {
	verifier = "test-verifier-string-of-sufficient-length-42"
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// registerClient runs DCR and returns the issued client_id.
func registerClient(t *testing.T, ts *httptest.Server, redirectURIs ...string) string {
	return registerNamedClient(t, ts, "Test Client", redirectURIs...)
}

func registerNamedClient(t *testing.T, ts *httptest.Server, name string, redirectURIs ...string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"redirect_uris": redirectURIs,
		"client_name":   name,
	})
	require.NoError(t, err)
	resp, err := http.Post(ts.URL+"/register", "application/json", strings.NewReader(string(body)))
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var reg oauthex.ClientRegistrationResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&reg))
	require.True(t, strings.HasPrefix(reg.ClientID, prefixClientID))
	return reg.ClientID
}

var loginIDRe = regexp.MustCompile(`/login/qr\?login=([0-9a-f]{32})`)

// startAuthorize drives GET /authorize and returns the loginID parsed out of
// the rendered QR page.
func startAuthorize(t *testing.T, ts *httptest.Server, clientID, challenge, state string) string {
	t.Helper()
	resp, err := http.Get(ts.URL + "/authorize?" + url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {testRedirectURI},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}.Encode())
	require.NoError(t, err)
	page, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "authorize should render the QR page: %s", page)
	assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'")
	assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	m := loginIDRe.FindSubmatch(page)
	require.NotNil(t, m, "QR page should reference the login id")
	return string(m[1])
}

// pollLogin fetches /login/poll once and decodes the JSON.
func pollLogin(t *testing.T, ts *httptest.Server, loginID string) pollResponse {
	t.Helper()
	resp, err := http.Get(ts.URL + "/login/poll?login=" + url.QueryEscape(loginID))
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var pr pollResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&pr))
	return pr
}

// finishLogin completes the fake flow and polls until the login is
// finalized, returning the code and state delivered to the redirect URI.
func finishLogin(t *testing.T, ts *httptest.Server, loginID string, flow *fakeFlow, user LoginUser, session []byte) (code, gotState string) {
	t.Helper()
	flow.complete(user, session)
	pr := pollLogin(t, ts, loginID)
	require.Equal(t, "done", pr.Status, "poll should report done: %+v", pr)
	loc, err := url.Parse(pr.Redirect)
	require.NoError(t, err)
	require.Truef(t, strings.HasPrefix(pr.Redirect, testRedirectURI), "redirect must target the client, got %s", pr.Redirect)
	require.Empty(t, loc.Query().Get("error"))
	return loc.Query().Get("code"), loc.Query().Get("state")
}

// redeemCode exchanges an authorization code at /token.
func redeemCode(t *testing.T, ts *httptest.Server, clientID, testRedirectURI, code, verifier string) (*tokenResponse, *oauthErrorResponse, int) {
	t.Helper()
	return postToken(t, ts, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {verifier},
	})
}

// refreshGrant redeems a refresh token at /token.
func refreshGrant(t *testing.T, ts *httptest.Server, clientID, refreshToken string) (*tokenResponse, *oauthErrorResponse, int) {
	t.Helper()
	return postToken(t, ts, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	})
}

func postToken(t *testing.T, ts *httptest.Server, form url.Values) (*tokenResponse, *oauthErrorResponse, int) {
	t.Helper()
	resp, err := http.PostForm(ts.URL+"/token", form)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	if resp.StatusCode == http.StatusOK {
		var tr tokenResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&tr))
		return &tr, nil, resp.StatusCode
	}
	var oe oauthErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&oe))
	return nil, &oe, resp.StatusCode
}

func TestMetadataEndpoints(t *testing.T) {
	_, ts := newTestServer(t, testConfig(t), sessionstore.NewMemory(), neverStartLogin)

	var prm oauthex.ProtectedResourceMetadata
	resp, err := http.Get(ts.URL + "/.well-known/oauth-protected-resource")
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck // test cleanup
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&prm))
	assert.Equal(t, testIssuer, prm.Resource)
	assert.Equal(t, []string{testIssuer}, prm.AuthorizationServers)

	var meta oauthex.AuthServerMeta
	resp2, err := http.Get(ts.URL + "/.well-known/oauth-authorization-server")
	require.NoError(t, err)
	defer resp2.Body.Close() //nolint:errcheck // test cleanup
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&meta))
	assert.Equal(t, testIssuer, meta.Issuer)
	assert.Equal(t, testIssuer+"/authorize", meta.AuthorizationEndpoint)
	assert.Equal(t, testIssuer+"/token", meta.TokenEndpoint)
	assert.Equal(t, testIssuer+"/register", meta.RegistrationEndpoint)
	assert.Equal(t, []string{"authorization_code", "refresh_token"}, meta.GrantTypesSupported)
	assert.Equal(t, []string{"S256"}, meta.CodeChallengeMethodsSupported)
	assert.Equal(t, []string{"none"}, meta.TokenEndpointAuthMethodsSupported)

	resp3, err := http.Get(ts.URL + "/jwks.json")
	require.NoError(t, err)
	defer resp3.Body.Close() //nolint:errcheck // test cleanup
	body, err := io.ReadAll(resp3.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"keys":[]}`, string(body))
}

func TestFullAuthorizationFlow(t *testing.T) {
	store := sessionstore.NewMemory()
	flow := newFakeFlow()
	a, ts := newTestServer(t, testConfig(t), store, startOne(flow))
	sessionBytes := []byte("gotd-session-bytes")

	clientID := registerClient(t, ts, testRedirectURI)
	verifier, challenge := pkcePair()
	loginID := startAuthorize(t, ts, clientID, challenge, "client-state-1")

	// Before the scan the poll reports waiting and announces the QR revision.
	pr := pollLogin(t, ts, loginID)
	assert.Equal(t, "waiting", pr.Status)
	assert.Equal(t, 1, pr.QRRev)

	// The QR endpoint serves a PNG of the current token URL.
	qrResp, err := http.Get(ts.URL + "/login/qr?login=" + loginID)
	require.NoError(t, err)
	qrBody, err := io.ReadAll(qrResp.Body)
	require.NoError(t, err)
	_ = qrResp.Body.Close()
	require.Equal(t, http.StatusOK, qrResp.StatusCode)
	assert.Equal(t, "image/png", qrResp.Header.Get("Content-Type"))
	assert.Equal(t, "no-store", qrResp.Header.Get("Cache-Control"))
	assert.True(t, len(qrBody) > 8 && string(qrBody[1:4]) == "PNG", "QR endpoint must serve a PNG")

	// A rotated Telegram token bumps qr_rev so the page re-fetches the image.
	flow.setURL("tg://login?token=rotated")
	pr = pollLogin(t, ts, loginID)
	assert.Equal(t, "waiting", pr.Status)
	assert.Equal(t, 2, pr.QRRev)

	// Scan confirmed: the poll finalizes the login and hands out the code.
	code, gotState := finishLogin(t, ts, loginID, flow,
		LoginUser{ID: allowedUser, Username: "durov"}, sessionBytes)
	assert.Equal(t, "client-state-1", gotState)
	assert.True(t, strings.HasPrefix(code, prefixCode))

	// The Telegram session was persisted under its own random session id and
	// per-session key, both carried in the code.
	cc, err := openBlob(a.sealer, codeBlob, code, a.now())
	require.NoError(t, err)
	require.True(t, validSessionID(cc.SessionID), "code must carry a well-formed session id")
	require.Len(t, cc.SessionKey, sessionKeyLen)
	stored, err := store.Session(allowedUser, cc.SessionID, cc.SessionKey).LoadSession(context.Background())
	require.NoError(t, err)
	assert.Equal(t, sessionBytes, stored)

	// The pending login is consumed: subsequent polls report expired.
	assert.Equal(t, "expired", pollLogin(t, ts, loginID).Status)

	// Code exchange with PKCE.
	tr, _, status := redeemCode(t, ts, clientID, testRedirectURI, code, verifier)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "Bearer", tr.TokenType)
	assert.True(t, strings.HasPrefix(tr.AccessToken, prefixAccess))
	assert.True(t, strings.HasPrefix(tr.RefreshToken, prefixRefresh))
	assert.Positive(t, tr.ExpiresIn)

	// The verifier accepts the access token and exposes the identity.
	info, err := a.Verifier()(context.Background(), tr.AccessToken, nil)
	require.NoError(t, err)
	assert.Equal(t, allowedUser.String(), info.UserID)
	extra, ok := info.Extra[extraIdentityKey].(identityExtra)
	require.True(t, ok)
	assert.Equal(t, allowedUser, extra.ID)
	assert.Equal(t, "durov", extra.Username)
	// The access token carries the same independent-session identity as the code.
	assert.Equal(t, cc.SessionID, extra.SessionID)
	assert.Equal(t, cc.SessionKey, extra.SessionKey)
	assert.False(t, info.Expiration.IsZero())

	// Refresh grant yields a fresh pair while the session exists.
	refreshed, _, status := refreshGrant(t, ts, clientID, tr.RefreshToken)
	require.Equal(t, http.StatusOK, status)
	info2, err := a.Verifier()(context.Background(), refreshed.AccessToken, nil)
	require.NoError(t, err)
	assert.Equal(t, allowedUser.String(), info2.UserID)

	// Deleting this authorization's Telegram session kills the refresh grant.
	rc, err := openBlob(a.sealer, refreshBlob, refreshed.RefreshToken, a.now())
	require.NoError(t, err)
	require.NoError(t, store.Delete(context.Background(), allowedUser, rc.SessionID))
	_, oe, status := refreshGrant(t, ts, clientID, refreshed.RefreshToken)
	require.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "invalid_grant", oe.Error)
}

func TestAuthorizeRejections(t *testing.T) {
	_, ts := newTestServer(t, testConfig(t), sessionstore.NewMemory(), startOne(newFakeFlow(), newFakeFlow()))
	clientID := registerClient(t, ts, testRedirectURI)
	_, challenge := pkcePair()
	client := noRedirectClient()

	get := func(v url.Values) *http.Response {
		resp, err := client.Get(ts.URL + "/authorize?" + v.Encode())
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp
	}
	base := func() url.Values {
		return url.Values{
			"client_id":             {clientID},
			"redirect_uri":          {testRedirectURI},
			"response_type":         {"code"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
		}
	}

	t.Run("bogus client_id renders error, no redirect", func(t *testing.T) {
		v := base()
		v.Set("client_id", "mcp_cid_forged.blob")
		resp := get(v)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Empty(t, resp.Header.Get("Location"))
	})
	t.Run("unregistered redirect_uri renders error, no redirect", func(t *testing.T) {
		v := base()
		v.Set("redirect_uri", "https://evil.example/cb")
		resp := get(v)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Empty(t, resp.Header.Get("Location"))
	})
	t.Run("missing PKCE redirects with invalid_request", func(t *testing.T) {
		v := base()
		v.Del("code_challenge")
		resp := get(v)
		require.Equal(t, http.StatusFound, resp.StatusCode)
		loc, err := url.Parse(resp.Header.Get("Location"))
		require.NoError(t, err)
		assert.Equal(t, "invalid_request", loc.Query().Get("error"))
	})
	t.Run("plain PKCE method redirects with invalid_request", func(t *testing.T) {
		v := base()
		v.Set("code_challenge_method", "plain")
		resp := get(v)
		require.Equal(t, http.StatusFound, resp.StatusCode)
		loc, err := url.Parse(resp.Header.Get("Location"))
		require.NoError(t, err)
		assert.Equal(t, "invalid_request", loc.Query().Get("error"))
	})
	t.Run("foreign resource redirects with invalid_target", func(t *testing.T) {
		v := base()
		v.Set("resource", "https://other-server.example")
		resp := get(v)
		require.Equal(t, http.StatusFound, resp.StatusCode)
		loc, err := url.Parse(resp.Header.Get("Location"))
		require.NoError(t, err)
		assert.Equal(t, "invalid_target", loc.Query().Get("error"))
	})
	t.Run("loopback port may differ from registration", func(t *testing.T) {
		v := base()
		v.Set("redirect_uri", "http://localhost:59999/callback")
		resp := get(v)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
	t.Run("wrong response_type redirects with unsupported_response_type", func(t *testing.T) {
		v := base()
		v.Set("response_type", "token")
		resp := get(v)
		require.Equal(t, http.StatusFound, resp.StatusCode)
		loc, err := url.Parse(resp.Header.Get("Location"))
		require.NoError(t, err)
		assert.Equal(t, "unsupported_response_type", loc.Query().Get("error"))
	})
}

func TestAuthorizeCapacityLimit(t *testing.T) {
	flows := make([]*fakeFlow, maxPendingLogins+1)
	for i := range flows {
		flows[i] = newFakeFlow()
	}
	_, ts := newTestServer(t, testConfig(t), sessionstore.NewMemory(), startOne(flows...))
	clientID := registerClient(t, ts, testRedirectURI)
	_, challenge := pkcePair()

	// Each authorize comes from a distinct client IP (X-Forwarded-For) so the
	// per-IP cap never trips before the global cap — this test targets the
	// global limit.
	authorize := func(ip, state string) *http.Response {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/authorize?"+url.Values{
			"client_id":             {clientID},
			"redirect_uri":          {testRedirectURI},
			"response_type":         {"code"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
			"state":                 {state},
		}.Encode(), nil)
		require.NoError(t, err)
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}

	for i := range maxPendingLogins {
		resp := authorize(fmt.Sprintf("10.0.0.%d", i), fmt.Sprintf("s-%d", i))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		_ = resp.Body.Close()
	}

	// One over the global cap (from yet another IP): 503, and the just-started
	// flow is aborted.
	resp := authorize("10.0.1.1", "over")
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.True(t, flows[maxPendingLogins].wasAborted(), "over-capacity flow must be aborted")
}

func TestAddPendingPerIPCap(t *testing.T) {
	a, _ := newTestServer(t, testConfig(t), sessionstore.NewMemory(), neverStartLogin)

	// A single IP may hold at most maxPendingLoginsPerIP concurrent slots —
	// well below the global cap, so one IP cannot lock everyone else out.
	for i := range maxPendingLoginsPerIP {
		_, err := a.addPending("blob", newFakeFlow(), "9.9.9.9")
		require.NoErrorf(t, err, "slot %d for one IP should be accepted", i)
	}
	if _, err := a.addPending("blob", newFakeFlow(), "9.9.9.9"); !errors.Is(err, errTooManyLogins) {
		t.Fatalf("over-cap request from the same IP = %v, want errTooManyLogins", err)
	}

	// A different IP is unaffected while global capacity remains.
	if _, err := a.addPending("blob", newFakeFlow(), "8.8.8.8"); err != nil {
		t.Fatalf("a different IP should still be admitted: %v", err)
	}
}

func TestRegisterRejections(t *testing.T) {
	_, ts := newTestServer(t, testConfig(t), sessionstore.NewMemory(), neverStartLogin)

	post := func(body string) (*http.Response, *oauthex.ClientRegistrationError) {
		resp, err := http.Post(ts.URL+"/register", "application/json", strings.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		var regErr oauthex.ClientRegistrationError
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&regErr))
		return resp, &regErr
	}

	t.Run("no redirect_uris", func(t *testing.T) {
		resp, regErr := post(`{"client_name":"x"}`)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "invalid_redirect_uri", regErr.ErrorCode)
	})
	t.Run("non-loopback http", func(t *testing.T) {
		resp, regErr := post(`{"redirect_uris":["http://intranet.example/cb"]}`)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "invalid_redirect_uri", regErr.ErrorCode)
	})
	t.Run("unsupported grant type", func(t *testing.T) {
		resp, regErr := post(`{"redirect_uris":["http://localhost/cb"],"grant_types":["implicit"]}`)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		assert.Equal(t, "invalid_client_metadata", regErr.ErrorCode)
	})
	t.Run("claude.ai callback allowed by default", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/register", "application/json",
			strings.NewReader(`{"redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`))
		require.NoError(t, err)
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		assert.Equal(t, http.StatusCreated, resp.StatusCode)
	})
}

func TestLoginEndpoints(t *testing.T) {
	_, challenge := pkcePair()

	setup := func(t *testing.T, flow *fakeFlow) (*AuthServer, *httptest.Server, *sessionstore.Memory, string, string) {
		t.Helper()
		store := sessionstore.NewMemory()
		a, ts := newTestServer(t, testConfig(t), store, startOne(flow))
		clientID := registerClient(t, ts, testRedirectURI)
		loginID := startAuthorize(t, ts, clientID, challenge, "s")
		return a, ts, store, clientID, loginID
	}

	t.Run("unknown login id polls as expired and QR is 404", func(t *testing.T) {
		_, ts := newTestServer(t, testConfig(t), sessionstore.NewMemory(), neverStartLogin)
		assert.Equal(t, "expired", pollLogin(t, ts, strings.Repeat("ab", 16)).Status)

		resp, err := http.Get(ts.URL + "/login/qr?login=" + strings.Repeat("ab", 16))
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("disallowed account is rejected and session never stored", func(t *testing.T) {
		flow := newFakeFlow()
		_, ts, store, _, loginID := setup(t, flow)

		flow.complete(LoginUser{ID: forbiddenUser, Username: "intruder"}, []byte("session"))
		pr := pollLogin(t, ts, loginID)
		assert.Equal(t, "failed", pr.Status)
		assert.Contains(t, pr.Message, "not allowed")

		exists, err := store.Exists(context.Background(), forbiddenUser, "")
		require.NoError(t, err)
		assert.False(t, exists, "a rejected account's session must never be persisted")
		assert.True(t, flow.wasAborted())
	})

	t.Run("failed login reports failed and aborts", func(t *testing.T) {
		flow := newFakeFlow()
		_, ts, _, _, loginID := setup(t, flow)

		flow.failNow(errors.New("AUTH_TOKEN_EXPIRED"))
		pr := pollLogin(t, ts, loginID)
		assert.Equal(t, "failed", pr.Status)
		assert.NotEmpty(t, pr.Message)
		assert.True(t, flow.wasAborted())
		// The entry is gone afterwards.
		assert.Equal(t, "expired", pollLogin(t, ts, loginID).Status)
	})

	t.Run("2FA password branch", func(t *testing.T) {
		flow := newFakeFlow()
		a, ts, store, clientID, loginID := setup(t, flow)

		flow.needPassword("hunter2", LoginUser{ID: allowedUser, Username: "durov"}, []byte("session-2fa"))
		pr := pollLogin(t, ts, loginID)
		assert.Equal(t, "password", pr.Status)
		assert.Empty(t, pr.Message, "no error before the first attempt")

		submit := func(pw string) int {
			resp, err := http.PostForm(ts.URL+"/login/password", url.Values{
				"login": {loginID}, "password": {pw},
			})
			require.NoError(t, err)
			_ = resp.Body.Close()
			return resp.StatusCode
		}

		// Wrong password: state stays password, poll carries the hint.
		assert.Equal(t, http.StatusNoContent, submit("wrong"))
		pr = pollLogin(t, ts, loginID)
		assert.Equal(t, "password", pr.Status)
		assert.NotEmpty(t, pr.Message)

		// Correct password completes the login.
		assert.Equal(t, http.StatusNoContent, submit("hunter2"))
		pr = pollLogin(t, ts, loginID)
		require.Equal(t, "done", pr.Status)
		assert.Equal(t, []string{"wrong", "hunter2"}, flow.passwords)

		// The session was persisted under its own random session id (carried in
		// the minted code).
		loc, err := url.Parse(pr.Redirect)
		require.NoError(t, err)
		code := loc.Query().Get("code")
		cc, err := openBlob(a.sealer, codeBlob, code, a.now())
		require.NoError(t, err)
		exists, err := store.Exists(context.Background(), allowedUser, cc.SessionID)
		require.NoError(t, err)
		assert.True(t, exists)

		// The minted code is redeemable.
		verifier, _ := pkcePair()
		_, _, status := redeemCode(t, ts, clientID, testRedirectURI, code, verifier)
		assert.Equal(t, http.StatusOK, status)
	})

	t.Run("password submit for unknown login is 404", func(t *testing.T) {
		_, ts := newTestServer(t, testConfig(t), sessionstore.NewMemory(), neverStartLogin)
		resp, err := http.PostForm(ts.URL+"/login/password", url.Values{
			"login": {strings.Repeat("cd", 16)}, "password": {"x"},
		})
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("TTL expiry aborts the flow", func(t *testing.T) {
		flow := newFakeFlow()
		a, ts, _, _, loginID := setup(t, flow)

		a.now = func() time.Time { return time.Now().Add(pendingLoginTTL + time.Minute) }
		assert.Equal(t, "expired", pollLogin(t, ts, loginID).Status)
		assert.True(t, flow.wasAborted())
	})

	t.Run("janitor sweep aborts expired flows", func(t *testing.T) {
		flow := newFakeFlow()
		a, _, _, _, _ := setup(t, flow)

		a.now = func() time.Time { return time.Now().Add(pendingLoginTTL + time.Minute) }
		a.sweepExpiredPending()
		assert.True(t, flow.wasAborted())
	})

	t.Run("Close aborts in-flight logins", func(t *testing.T) {
		flow := newFakeFlow()
		a, _, _, _, _ := setup(t, flow)

		a.Close()
		assert.True(t, flow.wasAborted())
	})
}

func TestTokenRejections(t *testing.T) {
	verifier, challenge := pkcePair()

	store := sessionstore.NewMemory()
	// Enough scripted flows for every fresh code this test needs.
	flows := make([]*fakeFlow, 8)
	for i := range flows {
		flows[i] = newFakeFlow()
	}
	next := 0
	a, ts := newTestServer(t, testConfig(t), store, startOne(flows...))
	clientID := registerClient(t, ts, testRedirectURI)

	freshCode := func(t *testing.T) string {
		t.Helper()
		flow := flows[next]
		next++
		loginID := startAuthorize(t, ts, clientID, challenge, "s")
		code, _ := finishLogin(t, ts, loginID, flow,
			LoginUser{ID: allowedUser, Username: "durov"}, []byte("session"))
		return code
	}

	t.Run("wrong verifier", func(t *testing.T) {
		_, oe, status := redeemCode(t, ts, clientID, testRedirectURI, freshCode(t), "wrong-verifier")
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, "invalid_grant", oe.Error)
	})
	t.Run("wrong client_id", func(t *testing.T) {
		otherClient := registerNamedClient(t, ts, "Another Client", testRedirectURI)
		require.NotEqual(t, clientID, otherClient)
		_, oe, status := redeemCode(t, ts, otherClient, testRedirectURI, freshCode(t), verifier)
		require.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, "invalid_grant", oe.Error)
	})
	t.Run("wrong redirect_uri", func(t *testing.T) {
		_, oe, status := redeemCode(t, ts, clientID, "http://localhost:1/cb", freshCode(t), verifier)
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, "invalid_grant", oe.Error)
	})
	t.Run("garbage code", func(t *testing.T) {
		_, oe, status := redeemCode(t, ts, clientID, testRedirectURI, "mcp_ac_garbage", verifier)
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, "invalid_grant", oe.Error)
	})
	t.Run("expired code", func(t *testing.T) {
		code := freshCode(t)
		a.now = func() time.Time { return time.Now().Add(2 * codeTTL) }
		t.Cleanup(func() { a.now = time.Now })
		_, oe, status := redeemCode(t, ts, clientID, testRedirectURI, code, verifier)
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, "invalid_grant", oe.Error)
	})
	t.Run("unsupported grant", func(t *testing.T) {
		resp, err := http.PostForm(ts.URL+"/token", url.Values{"grant_type": {"password"}})
		require.NoError(t, err)
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("refresh gates", func(t *testing.T) {
		code := freshCode(t)
		tr, _, status := redeemCode(t, ts, clientID, testRedirectURI, code, verifier)
		require.Equal(t, http.StatusOK, status)

		t.Run("garbage refresh token", func(t *testing.T) {
			_, oe, status := refreshGrant(t, ts, clientID, "mcp_rt_garbage")
			assert.Equal(t, http.StatusBadRequest, status)
			assert.Equal(t, "invalid_grant", oe.Error)
		})
		t.Run("client_id mismatch", func(t *testing.T) {
			otherClient := registerNamedClient(t, ts, "Third Client", testRedirectURI)
			_, oe, status := refreshGrant(t, ts, otherClient, tr.RefreshToken)
			assert.Equal(t, http.StatusBadRequest, status)
			assert.Equal(t, "invalid_grant", oe.Error)
		})
		t.Run("user removed from allowlist", func(t *testing.T) {
			a.cfg.Allow = AllowUsers(forbiddenUser)
			t.Cleanup(func() { a.cfg.Allow = AllowUsers(allowedUser) })
			_, oe, status := refreshGrant(t, ts, clientID, tr.RefreshToken)
			assert.Equal(t, http.StatusBadRequest, status)
			assert.Equal(t, "invalid_grant", oe.Error)
		})
		t.Run("absolute TTL anchored at login", func(t *testing.T) {
			// A refreshed pair does not extend the 30-day window: shifting
			// past LoginAt+TTL invalidates even a freshly minted token.
			refreshed, _, status := refreshGrant(t, ts, clientID, tr.RefreshToken)
			require.Equal(t, http.StatusOK, status)
			a.now = func() time.Time { return time.Now().Add(defaultRefreshTokenTTL + time.Hour) }
			t.Cleanup(func() { a.now = time.Now })
			_, oe, status := refreshGrant(t, ts, clientID, refreshed.RefreshToken)
			assert.Equal(t, http.StatusBadRequest, status)
			assert.Equal(t, "invalid_grant", oe.Error)
			assert.Contains(t, oe.ErrorDescription, "expired")
		})
	})
}

func TestVerifierRejections(t *testing.T) {
	verifier, challenge := pkcePair()
	flow := newFakeFlow()
	a, ts := newTestServer(t, testConfig(t), sessionstore.NewMemory(), startOne(flow))
	clientID := registerClient(t, ts, testRedirectURI)
	loginID := startAuthorize(t, ts, clientID, challenge, "s")
	code, _ := finishLogin(t, ts, loginID, flow,
		LoginUser{ID: allowedUser, Username: "durov"}, []byte("session"))
	tr, _, status := redeemCode(t, ts, clientID, testRedirectURI, code, verifier)
	require.Equal(t, http.StatusOK, status)

	t.Run("garbage token", func(t *testing.T) {
		_, err := a.Verifier()(context.Background(), "mcp_at_garbage", nil)
		assert.ErrorIs(t, err, auth.ErrInvalidToken)
	})
	t.Run("refresh token is not an access token", func(t *testing.T) {
		_, err := a.Verifier()(context.Background(), tr.RefreshToken, nil)
		assert.ErrorIs(t, err, auth.ErrInvalidToken)
	})
	t.Run("expired token", func(t *testing.T) {
		a.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
		t.Cleanup(func() { a.now = time.Now })
		_, err := a.Verifier()(context.Background(), tr.AccessToken, nil)
		assert.ErrorIs(t, err, auth.ErrInvalidToken)
	})
	t.Run("user removed from allowlist", func(t *testing.T) {
		a.cfg.Allow = AllowUsers(forbiddenUser)
		t.Cleanup(func() { a.cfg.Allow = AllowUsers(allowedUser) })
		_, err := a.Verifier()(context.Background(), tr.AccessToken, nil)
		assert.ErrorIs(t, err, auth.ErrInvalidToken)
	})
}

func TestRevokeIsSpecCompliantNoOp(t *testing.T) {
	_, ts := newTestServer(t, testConfig(t), sessionstore.NewMemory(), neverStartLogin)

	for _, token := range []string{"mcp_rt_whatever", "garbage", ""} {
		resp, err := http.PostForm(ts.URL+"/revoke", url.Values{"token": {token}})
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	}
}

func TestIdentityFromContext(t *testing.T) {
	t.Run("absent on plain context", func(t *testing.T) {
		_, ok := Identity(context.Background())
		assert.False(t, ok)
	})
}

// TestRefreshCarriesSessionIdentity pins that a session's sid and per-session
// key ride verbatim through every refresh, so a refreshed token keeps pointing
// at (and decrypting) the same session object.
func TestRefreshCarriesSessionIdentity(t *testing.T) {
	store := sessionstore.NewMemory()
	flow := newFakeFlow()
	a, ts := newTestServer(t, testConfig(t), store, startOne(flow))

	clientID := registerClient(t, ts, testRedirectURI)
	verifier, challenge := pkcePair()
	loginID := startAuthorize(t, ts, clientID, challenge, "s")
	code, _ := finishLogin(t, ts, loginID, flow, LoginUser{ID: allowedUser, Username: "durov"}, []byte("sess"))

	tr, _, status := redeemCode(t, ts, clientID, testRedirectURI, code, verifier)
	require.Equal(t, http.StatusOK, status)

	cc, err := openBlob(a.sealer, codeBlob, code, a.now())
	require.NoError(t, err)
	require.True(t, validSessionID(cc.SessionID))

	// Two consecutive refreshes must preserve the exact sid and key.
	rt := tr.RefreshToken
	for i := range 2 {
		refreshed, _, st := refreshGrant(t, ts, clientID, rt)
		require.Equalf(t, http.StatusOK, st, "refresh %d", i)

		ac, err := openBlob(a.sealer, accessBlob, refreshed.AccessToken, a.now())
		require.NoError(t, err)
		assert.Equalf(t, cc.SessionID, ac.SessionID, "access sid after refresh %d", i)
		assert.Equalf(t, cc.SessionKey, ac.SessionKey, "access key after refresh %d", i)

		rc, err := openBlob(a.sealer, refreshBlob, refreshed.RefreshToken, a.now())
		require.NoError(t, err)
		assert.Equalf(t, cc.SessionID, rc.SessionID, "refresh sid after refresh %d", i)
		assert.Equalf(t, cc.SessionKey, rc.SessionKey, "refresh key after refresh %d", i)
		rt = refreshed.RefreshToken
	}
}

// TestLegacySessionUpgradeOnRefresh pins the migration path: a pre-upgrade
// refresh token (no sid/sk) pointing at a legacy master-only session is
// upgraded, on refresh, to an independent split-key session — the new tokens
// carry a fresh sid+key, a new v2 object is written, and the legacy object is
// left in place.
func TestLegacySessionUpgradeOnRefresh(t *testing.T) {
	store := sessionstore.NewMemory()
	a, ts := newTestServer(t, testConfig(t), store, neverStartLogin)
	clientID := registerClient(t, ts, testRedirectURI)

	// Seed a legacy session (empty sid) and a matching legacy refresh token.
	require.NoError(t, store.Session(allowedUser, "", nil).StoreSession(context.Background(), []byte("legacy-session")))
	now := a.now()
	legacyRefresh, err := sealBlob(a.sealer, refreshBlob, refreshClaims{
		Subject:  allowedUser.String(),
		Username: "durov",
		ClientID: clientID,
		IssuedAt: now.Unix(),
		LoginAt:  now.Unix(),
	})
	require.NoError(t, err)

	refreshed, _, status := refreshGrant(t, ts, clientID, legacyRefresh)
	require.Equal(t, http.StatusOK, status)

	// The new tokens carry a fresh independent-session identity.
	rc, err := openBlob(a.sealer, refreshBlob, refreshed.RefreshToken, a.now())
	require.NoError(t, err)
	require.True(t, validSessionID(rc.SessionID), "upgraded refresh must carry a real sid")
	require.Len(t, rc.SessionKey, sessionKeyLen)

	// The upgraded session exists as its own object; the legacy one is kept.
	exists, err := store.Exists(context.Background(), allowedUser, rc.SessionID)
	require.NoError(t, err)
	assert.True(t, exists, "upgraded v2 session object must be written")
	legacyExists, err := store.Exists(context.Background(), allowedUser, "")
	require.NoError(t, err)
	assert.True(t, legacyExists, "legacy object must be left in place for other clients / age GC")

	// The upgraded session decrypts with the new key and holds the same bytes.
	got, err := store.Session(allowedUser, rc.SessionID, rc.SessionKey).LoadSession(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []byte("legacy-session"), got)
}

func TestNewTokenInfoForTesting(t *testing.T) {
	uk := []byte("test-session-key")
	info := NewTokenInfoForTesting(allowedUser, "durov", "deadbeef", uk, time.Now().Add(time.Hour))
	assert.Equal(t, allowedUser.String(), info.UserID)
	extra, ok := info.Extra[extraIdentityKey].(identityExtra)
	require.True(t, ok)
	assert.Equal(t, allowedUser, extra.ID)
	assert.Equal(t, "durov", extra.Username)
	assert.Equal(t, "deadbeef", extra.SessionID)
	assert.Equal(t, uk, extra.SessionKey)
}
