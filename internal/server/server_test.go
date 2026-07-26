package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

func TestNewRequiresConfig(t *testing.T) {
	_, err := New(Options{Version: "test"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Config is required")
}

func TestNewAppliesDefaultStdio(t *testing.T) {
	srv, err := New(Options{
		Config:  &tgclient.Config{APIID: 1, APIHash: "hash"},
		Version: "test",
	})
	require.NoError(t, err)
	assert.NotNil(t, srv.stdin)
	assert.NotNil(t, srv.stdout)
	assert.NotNil(t, srv.errOut)
}

// connectViaRun drives the full Server.Run entry point, so the tests that use
// it also pin that Run routes an unusable config into login-required mode.
// Only safe with a config Run cannot try to connect with — anything else
// reaches for a real Telegram DC.
func connectViaRun(t *testing.T, cfg *tgclient.Config, tweak func(*Server)) *mcp.ClientSession {
	t.Helper()
	return connectServer(t, cfg, tweak, (*Server).Run)
}

// connectLoginRequired enters the mode directly with a given reason, for the
// tool-behaviour tests: they need credentials present (so the probe branch is
// reached) without Run trying to use them against the network.
func connectLoginRequired(t *testing.T, cfg *tgclient.Config, reason string, tweak func(*Server)) *mcp.ClientSession {
	t.Helper()
	return connectServer(t, cfg, tweak, func(s *Server, ctx context.Context) error {
		return s.runLoginRequired(ctx, reason)
	})
}

// connectServer starts a server over a pair of pipes and connects a real MCP
// client to the other end, so the tests exercise the same initialize handshake
// a host performs rather than hand-rolled frames. tweak, when non-nil, adjusts
// the Server before it starts (e.g. to inject an auth probe).
func connectServer(t *testing.T, cfg *tgclient.Config, tweak func(*Server), start func(*Server, context.Context) error) *mcp.ClientSession {
	t.Helper()
	ctx := t.Context()

	clientR, serverW := io.Pipe() // server → client
	serverR, clientW := io.Pipe() // client → server

	srv, err := New(Options{
		Config:  cfg,
		Version: "test",
		Stdin:   serverR,
		Stdout:  serverW,
		ErrOut:  &bytes.Buffer{},
	})
	require.NoError(t, err)
	if tweak != nil {
		tweak(srv)
	}

	// Run's error is captured rather than ignored: if login-required mode ever
	// fails to come up, Connect below would otherwise block until t.Context is
	// cancelled at teardown, turning a failure into a package-level timeout
	// panic. The run itself only ends at teardown, so the value is read only
	// when Connect gave up first.
	runErr := make(chan error, 1)
	go func() { runErr <- start(srv, ctx) }()

	connectCtx, cancelConnect := context.WithTimeout(ctx, 5*time.Second)
	defer cancelConnect()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(connectCtx, &mcp.IOTransport{Reader: clientR, Writer: clientW}, nil)
	if err != nil {
		select {
		case e := <-runErr:
			t.Fatalf("connect failed (%v); the server exited with: %v", err, e)
		default:
			t.Fatalf("connect failed: %v", err)
		}
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = clientW.Close()
	})
	return cs
}

// staticProbe builds an auth-probe stub for the login-required tool.
func staticProbe(account string, authorized bool, err error) func(context.Context) (string, bool, error) {
	return func(context.Context) (string, bool, error) { return account, authorized, err }
}

// callLoginTool invokes the login-required tool and decodes its structured
// output, asserting the call itself reported success.
func callLoginTool(t *testing.T, cs *mcp.ClientSession) LoginRequiredStatus {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      loginRequiredTool,
		Arguments: map[string]any{},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, "the tool reports state, it does not fail")

	encoded, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	var status LoginRequiredStatus
	require.NoError(t, json.Unmarshal(encoded, &status))
	return status
}

// TestRunMissingCredentialsConnects pins the behaviour change: an unusable
// Telegram config must produce a *connected* server, because a host renders a
// failed stdio connection as a bare "failed" with no reason attached.
func TestRunMissingCredentialsConnects(t *testing.T) {
	cs := connectViaRun(t, &tgclient.Config{}, nil)

	init := cs.InitializeResult()
	require.NotNil(t, init)
	// The phrasing is the contract with the model, so it is asserted
	// literally; the reason itself is asserted through its constant so a
	// reworded message cannot silently stop being propagated.
	assert.Contains(t, init.Instructions, "NOT connected to Telegram")
	assert.Contains(t, init.Instructions, missingCredentialsMessage)
	assert.Contains(t, init.Instructions, loginRequiredTool)
}

// TestRunMissingCredentialsExposesOnlyTheLoginTool guards the other half of
// the contract: no Telegram tool may be advertised when Telegram is
// unreachable, or the model will call one and get an opaque failure.
func TestRunMissingCredentialsExposesOnlyTheLoginTool(t *testing.T) {
	cs := connectViaRun(t, &tgclient.Config{}, nil)

	res, err := cs.ListTools(t.Context(), &mcp.ListToolsParams{})
	require.NoError(t, err)
	require.Len(t, res.Tools, 1)
	assert.Equal(t, loginRequiredTool, res.Tools[0].Name)
	assert.Contains(t, res.Tools[0].Description, "NOT connected to Telegram")
	assert.Contains(t, res.Tools[0].Description, missingCredentialsMessage)
}

// TestLoginRequiredModeExposesNoResourcesOrPrompts pins that the happy path's
// resource/prompt wiring stays out of this mode. Those handlers are all built
// on a live client.API(); advertising them here would hand the model calls
// that dereference a client this process does not have.
func TestLoginRequiredModeExposesNoResourcesOrPrompts(t *testing.T) {
	cs := connectViaRun(t, &tgclient.Config{}, nil)

	resources, err := cs.ListResources(t.Context(), &mcp.ListResourcesParams{})
	require.NoError(t, err)
	assert.Empty(t, resources.Resources)

	templates, err := cs.ListResourceTemplates(t.Context(), &mcp.ListResourceTemplatesParams{})
	require.NoError(t, err)
	assert.Empty(t, templates.ResourceTemplates)

	prompts, err := cs.ListPrompts(t.Context(), &mcp.ListPromptsParams{})
	require.NoError(t, err)
	assert.Empty(t, prompts.Prompts)
}

// TestLoginRequiredToolReportsMissingCredentials covers the one state that
// needs no Telegram contact: tgConfig is frozen at startup, so the verdict
// cannot change in-process and the detail has to say a reconnect is required.
func TestLoginRequiredToolReportsMissingCredentials(t *testing.T) {
	cs := connectViaRun(t, &tgclient.Config{}, func(s *Server) {
		s.authProbeFn = staticProbe("", false, errors.New("probe must not run without credentials"))
	})

	status := callLoginTool(t, cs)
	assert.Equal(t, StateNotConfigured, status.State)
	assert.False(t, status.Authorized)
	assert.False(t, status.TelegramToolsAvailable)
	assert.Contains(t, status.FixCommand, "config set api-id")
	assert.Contains(t, status.Detail, "reconnect this MCP server")
}

// TestLoginRequiredToolProbedStates exercises the three states that depend on
// a live Telegram check, through the injected probe. Without the seam these
// branches are unreachable in tests: authProbe reaches the OS keychain and a
// real DC.
func TestLoginRequiredToolProbedStates(t *testing.T) {
	cfg := &tgclient.Config{APIID: 1, APIHash: "hash"}

	tests := []struct {
		name        string
		probe       func(context.Context) (string, bool, error)
		wantState   LoginState
		wantAuthzd  bool
		wantFix     bool
		wantDetail  string
		wantStartup bool // startup_reason repeated alongside the live detail
	}{
		{
			name:       "session still not authorized",
			probe:      staticProbe("", false, nil),
			wantState:  StateLoginRequired,
			wantFix:    true,
			wantDetail: "not logged in to Telegram",
			// Detail *is* the startup reason here, so repeating it would send
			// the model the same paragraph twice.
			wantStartup: false,
		},
		{
			name:        "probe could not determine anything",
			probe:       staticProbe("", false, errors.New("dial tcp: no route to host")),
			wantState:   StateCheckFailed,
			wantFix:     true,
			wantDetail:  "no route to host",
			wantStartup: true,
		},
		{
			name:        "logged in elsewhere since startup",
			probe:       staticProbe("Ada Lovelace", true, nil),
			wantState:   StateAuthorizedPendingReconnect,
			wantAuthzd:  true,
			wantDetail:  "Ada Lovelace",
			wantStartup: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := connectLoginRequired(t, cfg, notLoggedInMessage, func(s *Server) { s.authProbeFn = tc.probe })

			status := callLoginTool(t, cs)
			assert.Equal(t, tc.wantState, status.State)
			assert.Equal(t, tc.wantAuthzd, status.Authorized,
				"authorized must be derived from state, never set independently")
			assert.False(t, status.TelegramToolsAvailable,
				"no Telegram tool is loaded in this mode whatever the session says")
			assert.Contains(t, status.Detail, tc.wantDetail)
			if tc.wantFix {
				assert.NotEmpty(t, status.FixCommand)
			} else {
				assert.Empty(t, status.FixCommand, "a host-side reconnect is not a shell command")
			}
			// StartupReason is the only place the original diagnosis survives
			// once the live detail describes something else.
			if tc.wantStartup {
				assert.Equal(t, notLoggedInMessage, status.StartupReason)
			} else {
				assert.Empty(t, status.StartupReason, "detail already carries the startup reason verbatim")
			}
		})
	}
}

// TestLoginRequiredToolRechecksLive is the tool's headline promise: it detects
// a login completed in another terminal, so two calls must be able to
// disagree. A refactor that computed the status once at startup would pass
// every other test in this file.
func TestLoginRequiredToolRechecksLive(t *testing.T) {
	var calls int
	cs := connectLoginRequired(t, &tgclient.Config{APIID: 1, APIHash: "hash"}, notLoggedInMessage, func(s *Server) {
		s.authProbeFn = func(context.Context) (string, bool, error) {
			calls++
			if calls == 1 {
				return "", false, nil
			}
			return "Ada Lovelace", true, nil
		}
	})

	assert.Equal(t, StateLoginRequired, callLoginTool(t, cs).State)

	second := callLoginTool(t, cs)
	assert.Equal(t, StateAuthorizedPendingReconnect, second.State)
	assert.True(t, second.Authorized)
	assert.Equal(t, "Ada Lovelace", second.Account)
}

// TestRunLoginRequiredExitsCleanlyOnStdinClose covers host shutdown: closing
// stdin is how a host stops a stdio server, and it must not look like a crash.
func TestRunLoginRequiredExitsCleanlyOnStdinClose(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	srv := newPipeServer(t, stdinR)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(t.Context()) }()

	require.NoError(t, stdinW.Close())

	select {
	case runErr := <-errCh:
		assert.NoError(t, runErr, "clean host disconnect must not exit non-zero")
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Run did not return within 3s on stdin close")
	}
}

// TestRunLoginRequiredExitsCleanlyOnContextCancel covers the other shutdown
// path. It also pins that Run does not wedge: stdioTransport hands the SDK a
// plain io.NopCloser with no cancellation path of its own, so whether a read
// parked on stdin unblocks is the SDK's business and could change under a
// dependency bump. A hang here would be a SIGINT that does not kill the
// process.
func TestRunLoginRequiredExitsCleanlyOnContextCancel(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	defer func() { _ = stdinW.Close() }()
	srv := newPipeServer(t, stdinR)

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	// Give the serve loop a moment to park on stdin, so cancel has something
	// to interrupt rather than racing the startup.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case runErr := <-errCh:
		assert.NoError(t, runErr, "SIGINT-shaped shutdown must not read as a crash")
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Run did not return within 3s after ctx cancel")
	}
}

func newPipeServer(t *testing.T, stdin io.Reader) *Server {
	t.Helper()
	srv, err := New(Options{
		Config:  &tgclient.Config{},
		Version: "test",
		Stdin:   stdin,
		Stdout:  &nopWriteCloser{Writer: io.Discard},
		ErrOut:  &bytes.Buffer{},
	})
	require.NoError(t, err)
	return srv
}

// TestFinishRunClassifiesByPhase is the regression guard for the bug this
// classification replaced: gotd reports a whole class of failures (corrupt
// session, AUTH_KEY_UNREGISTERED, SESSION_EXPIRED, any connect-phase 401)
// without ever running our callback, so no *blockedError exists for them.
// Keying on the sentinel alone sent exactly those cases — the revoked
// sessions this mode is for — to a hard exit.
func TestFinishRunClassifiesByPhase(t *testing.T) {
	// Over HTTP startBlocked returns the reason verbatim, which makes the
	// routing decision directly observable without standing up a session.
	newHTTP := func(t *testing.T) *Server {
		t.Helper()
		srv, err := New(Options{
			Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
			Version:   "test",
			Transport: TransportHTTP,
			HTTPAddr:  ":0",
			Stdin:     &bytes.Buffer{},
			Stdout:    &bytes.Buffer{},
			ErrOut:    &bytes.Buffer{},
		})
		require.NoError(t, err)
		return srv
	}

	t.Run("blocked error survives the client's wrapping", func(t *testing.T) {
		// Mirrors gotd: the callback error is wrapped ("callback: ...") and
		// may be aggregated with close errors before it reaches us.
		wrapped := fmt.Errorf("callback: %w", &blockedError{message: notLoggedInMessage})
		err := newHTTP(t).finishRun(t.Context(), wrapped, false)
		require.Error(t, err)
		assert.Equal(t, notLoggedInMessage, err.Error())
	})

	t.Run("connect-phase failure is blocked too", func(t *testing.T) {
		err := newHTTP(t).finishRun(t.Context(), errors.New("corrupted key"), false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "could not connect to Telegram")
		assert.Contains(t, err.Error(), "corrupted key")
		assert.Contains(t, err.Error(), "logout", "a corrupt or revoked session needs the logout/login recovery")
	})

	t.Run("failure after serving started is fatal", func(t *testing.T) {
		err := newHTTP(t).finishRun(t.Context(), errors.New("stream broke"), true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "running server")
		assert.Contains(t, err.Error(), "stream broke")
		assert.NotContains(t, err.Error(), "could not connect",
			"a mid-session failure must not be re-diagnosed as a startup problem")
	})

	t.Run("clean run stays clean", func(t *testing.T) {
		assert.NoError(t, newHTTP(t).finishRun(t.Context(), nil, true))
	})
}

// TestRunHTTPMissingCredentialsFailsFast pins the transport split end to end:
// over HTTP there is no MCP peer to tell, so the process must die rather than
// bind a listener it can never serve.
func TestRunHTTPMissingCredentialsFailsFast(t *testing.T) {
	srv, err := New(Options{
		Config:    &tgclient.Config{},
		Version:   "test",
		Transport: TransportHTTP,
		HTTPAddr:  ":0",
		Stdin:     &bytes.Buffer{},
		Stdout:    &bytes.Buffer{},
		ErrOut:    &bytes.Buffer{},
	})
	require.NoError(t, err)

	err = srv.Run(t.Context())
	require.Error(t, err)
	assert.Equal(t, missingCredentialsMessage, err.Error())
}

func TestLoginCommandUsesRunningBinaryPath(t *testing.T) {
	// The binary is normally wired into a host by absolute path and is not on
	// $PATH, so a bare `mcp-telegram login` would not be runnable.
	exe, err := os.Executable()
	require.NoError(t, err)
	assert.Contains(t, loginCommand(), exe)
	assert.Contains(t, loginCommand(), "login --phone")
	assert.Contains(t, configureCommand(), exe)
}

func TestIsTTYRejectsNonFile(t *testing.T) {
	// bytes.Buffer is not *os.File — must be treated as non-TTY so a blocked
	// start serves the login-required server instead of printing and exiting.
	assert.False(t, isTTY(&bytes.Buffer{}))
}

func TestIsTTYRejectsPipe(t *testing.T) {
	// os.Pipe returns *os.File but pipes are not character devices, so isTTY
	// must return false and take the login-required path.
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()
	assert.False(t, isTTY(r))
}

// TestFinishRunServesLoginRequiredOverStdio pins the delivery half of the
// classification. TestFinishRunClassifiesByPhase uses HTTP servers so the
// routing decision is observable as a returned error, which means it never
// shows that a blocked stdio start actually produces a *connected* server.
// This is the headline scenario of the whole mode — credentials present, the
// connect phase fails, and the host gets a live session instead of exit 1 —
// and Run's own early return for an empty config bypasses finishRun entirely,
// so no other test reaches this composition.
func TestFinishRunServesLoginRequiredOverStdio(t *testing.T) {
	ctx := t.Context()

	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()

	srv, err := New(Options{
		Config:  &tgclient.Config{APIID: 1, APIHash: "hash"},
		Version: "test",
		Stdin:   serverR,
		Stdout:  serverW,
		ErrOut:  &bytes.Buffer{},
	})
	require.NoError(t, err)
	srv.authProbeFn = staticProbe("", false, nil)

	// A connect-phase failure: no *blockedError, because gotd never ran our
	// callback (this is what "corrupted key" out of restoreConnection looks
	// like from here).
	go func() { _ = srv.finishRun(ctx, errors.New("corrupted key"), false) }()

	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(connectCtx, &mcp.IOTransport{Reader: clientR, Writer: clientW}, nil)
	require.NoError(t, err, "a connect-phase failure must yield a connected server, not a dead process")
	t.Cleanup(func() {
		_ = cs.Close()
		_ = clientW.Close()
	})

	tools, err := cs.ListTools(ctx, &mcp.ListToolsParams{})
	require.NoError(t, err)
	require.Len(t, tools.Tools, 1)
	assert.Equal(t, loginRequiredTool, tools.Tools[0].Name)
	assert.Contains(t, cs.InitializeResult().Instructions, "corrupted key",
		"the reason gotd reported has to reach the model")
}

// TestIsDeadSessionSeparatesRejectionFromUnreachable guards the distinction
// the re-check depends on. Telegram rejecting the stored key is a verdict
// ("log in again"); anything else is an undetermined check ("retry"). Getting
// this wrong is not cosmetic: a revoked session fails in the connect phase, so
// the callback never runs and there is no Status to read — without this test
// the tool reports check_failed and advises the user to check their network
// for a session they revoked themselves.
func TestIsDeadSessionSeparatesRejectionFromUnreachable(t *testing.T) {
	dead := []error{
		tgerr.New(401, "AUTH_KEY_UNREGISTERED"),
		tgerr.New(406, "SESSION_EXPIRED"),
		tgerr.New(406, "AUTH_KEY_DUPLICATED"),
		tgerr.New(401, "SESSION_REVOKED"),
		// Must survive the wrapping gotd applies on the way out.
		fmt.Errorf("callback: %w", tgerr.New(401, "AUTH_KEY_UNREGISTERED")),
	}
	for _, err := range dead {
		assert.True(t, isDeadSession(err), "expected a dead-session verdict for %v", err)
	}

	alive := []error{
		errors.New("dial tcp: no route to host"),
		context.DeadlineExceeded,
		tgerr.New(420, "FLOOD_WAIT_30"),
		tgerr.New(500, "INTERNAL_SERVER_ERROR"),
	}
	for _, err := range alive {
		assert.False(t, isDeadSession(err), "expected an undetermined check for %v", err)
	}
}
