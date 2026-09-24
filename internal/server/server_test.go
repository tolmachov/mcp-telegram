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

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tolmachov/mcp-telegram/internal/summarize"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

func TestNewRequiresConfig(t *testing.T) {
	_, err := New(Options{Version: "test"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Config is required")
}

// connector connects the local account in a test, standing in for
// Server.connectLocal.
type connector = func(context.Context) (localClient, error)

// newServerConnecting is newServer connecting the local account through
// connect.
func newServerConnecting(opts Options, connect connector) (*Server, error) {
	srv, err := newServer(opts)
	if err != nil {
		return nil, err
	}
	srv.connectLocal = connect
	return srv, nil
}

// connectViaRun drives the full Server.Run entry point, so the tests that use
// it also pin that Run routes an unusable config into login-required mode.
func connectViaRun(t *testing.T, cfg *tgclient.Config, connect connector) *mcp.ClientSession {
	t.Helper()
	return connectServer(t, cfg, connect, (*Server).Run)
}

// connectLoginRequired enters the mode directly with a given reason, for the
// tool-behaviour tests: they need credentials present (so the probe branch is
// reached) without Run trying to use them against the network.
func connectLoginRequired(t *testing.T, cfg *tgclient.Config, reason string, connect connector) *mcp.ClientSession {
	t.Helper()
	return connectServer(t, cfg, connect, func(s *Server, ctx context.Context) error {
		return s.runLoginRequired(ctx, reason)
	})
}

// connectServer starts a server over a pair of pipes and connects a real MCP
// client to the other end, so the tests exercise the same initialize handshake
// a host performs rather than hand-rolled frames. connect stands in for the
// local account's connection (and so for the login-required re-check).
func connectServer(t *testing.T, cfg *tgclient.Config, connect connector, start func(*Server, context.Context) error) *mcp.ClientSession {
	t.Helper()
	ctx := t.Context()

	clientR, serverW := io.Pipe() // server → client
	serverR, clientW := io.Pipe() // client → server

	srv, err := newServerConnecting(Options{
		Config:    cfg,
		Summarize: testSummarize,
		Version:   "test",
		Stdin:     serverR,
		Stdout:    serverW,
		ErrOut:    &bytes.Buffer{},
		Transport: TransportStdio,
	}, connect)
	require.NoError(t, err)

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

// noConnect is the local connection of a test that must never make one.
func noConnect(t *testing.T) connector {
	return func(context.Context) (localClient, error) {
		t.Error("the server connected to Telegram")
		return nil, errors.New("this test does not connect")
	}
}

// staticConnect connects the local account the same way every time: failing
// with err, refused by Telegram when not authorized, or else as fakeSelf.
func staticConnect(authorized bool, err error) connector {
	return func(context.Context) (localClient, error) {
		switch {
		case err != nil:
			return nil, err
		case !authorized:
			return nil, fmt.Errorf("starting Telegram client: %w", tgclient.ErrSessionUnauthorized)
		}
		return newFakeClient(), nil
	}
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
	cs := connectViaRun(t, &tgclient.Config{}, noConnect(t))

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
	cs := connectViaRun(t, &tgclient.Config{}, noConnect(t))

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
	cs := connectViaRun(t, &tgclient.Config{}, noConnect(t))

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
	cs := connectViaRun(t, &tgclient.Config{}, noConnect(t))

	status := callLoginTool(t, cs)
	assert.Equal(t, StateNotConfigured, status.State)
	assert.False(t, status.Authorized)
	assert.False(t, status.TelegramToolsAvailable)
	assert.Contains(t, status.FixCommand, "config set api-id")
	assert.Contains(t, status.Detail, "reconnect this MCP server")
}

// TestLoginRequiredToolProbedStates exercises the three states that depend on
// a live Telegram check, through the injected local connection. Without it
// these branches are unreachable in tests: the real one reaches the OS
// keychain and a real DC.
func TestLoginRequiredToolProbedStates(t *testing.T) {
	cfg := &tgclient.Config{APIID: 1, APIHash: "hash"}

	tests := []struct {
		name        string
		connect     connector
		wantState   LoginState
		wantAuthzd  bool
		wantFix     bool
		wantDetail  string
		wantStartup bool // startup_reason repeated alongside the live detail
	}{
		{
			name:       "session still not authorized",
			connect:    staticConnect(false, nil),
			wantState:  StateLoginRequired,
			wantFix:    true,
			wantDetail: "not logged in to Telegram",
			// Detail *is* the startup reason here, so repeating it would send
			// the model the same paragraph twice.
			wantStartup: false,
		},
		{
			name:        "probe could not determine anything",
			connect:     staticConnect(false, errors.New("dial tcp: no route to host")),
			wantState:   StateCheckFailed,
			wantFix:     true,
			wantDetail:  "no route to host",
			wantStartup: true,
		},
		{
			name:        "logged in elsewhere since startup",
			connect:     staticConnect(true, nil),
			wantState:   StateAuthorizedPendingReconnect,
			wantAuthzd:  true,
			wantDetail:  fakeSelf,
			wantStartup: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := connectLoginRequired(t, cfg, notLoggedInMessage, tc.connect)

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
	probed := newFakeClient()
	cs := connectLoginRequired(t, &tgclient.Config{APIID: 1, APIHash: "hash"}, notLoggedInMessage,
		func(context.Context) (localClient, error) {
			calls++
			if calls == 1 {
				return nil, fmt.Errorf("starting Telegram client: %w", tgclient.ErrSessionUnauthorized)
			}
			return probed, nil
		})

	assert.Equal(t, StateLoginRequired, callLoginTool(t, cs).State)

	second := callLoginTool(t, cs)
	assert.Equal(t, StateAuthorizedPendingReconnect, second.State)
	assert.True(t, second.Authorized)
	assert.Equal(t, fakeSelf, second.Account)
	assert.True(t, probed.isClosed(), "the re-check disconnects the client it connected")
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
	srv, err := newServerConnecting(Options{
		Config:    &tgclient.Config{},
		Summarize: testSummarize,
		Version:   "test",
		Stdin:     stdin,
		Stdout:    &nopWriteCloser{Writer: io.Discard},
		ErrOut:    &bytes.Buffer{},
		Transport: TransportStdio,
	}, noConnect(t))
	require.NoError(t, err)
	return srv
}

// TestBlockedReasonClassifiesStartupFailures is the regression guard for the
// routing Run applies to a failed connectLocal. gotd reports a whole class
// of failures (corrupt session, AUTH_KEY_UNREGISTERED, SESSION_EXPIRED, any
// connect-phase 401) without ever running the ready callback; StartClient
// folds the rejections into ErrSessionUnauthorized, and everything else must
// still name the raw cause.
func TestBlockedReasonClassifiesStartupFailures(t *testing.T) {
	t.Run("a refused session asks for a login", func(t *testing.T) {
		err := fmt.Errorf("starting Telegram client: %w", tgclient.ErrSessionUnauthorized)
		assert.Equal(t, notLoggedInMessage, blockedReason(err))
	})

	t.Run("a connect-phase failure names its cause", func(t *testing.T) {
		reason := blockedReason(errors.New("starting Telegram client: corrupted key"))
		assert.Contains(t, reason, "could not connect to Telegram")
		assert.Contains(t, reason, "corrupted key")
		assert.Contains(t, reason, "logout", "a corrupt or revoked session needs the logout/login recovery")
	})
}

// TestProbeVerdictSeparatesRejectionFromUndetermined pins the re-check's two
// quirks: a session Telegram refused before the auth check ran is still the
// verdict "not authorized", and a probe that ended without an answer (a
// cancelled run, which gotd reports as a nil error) is never one.
func TestProbeVerdictSeparatesRejectionFromUndetermined(t *testing.T) {
	account, authorized, err := probeVerdict(nil, fmt.Errorf("starting Telegram client: %w", tgclient.ErrSessionUnauthorized))
	require.NoError(t, err)
	assert.False(t, authorized)
	assert.Empty(t, account)

	for _, cause := range []error{context.Canceled, errors.New("telegram client exited during startup")} {
		_, authorized, err = probeVerdict(nil, cause)
		require.ErrorIs(t, err, cause)
		assert.False(t, authorized)
	}
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

// TestBlockedStartupServesLoginRequiredOverStdio pins the delivery half of
// the classification: credentials present, the connect phase fails, and the
// host gets a live session instead of exit 1. Run's own early return for an
// empty config never reaches blockedReason, so no other test covers this
// composition.
func TestBlockedStartupServesLoginRequiredOverStdio(t *testing.T) {
	// What "corrupted key" out of restoreConnection looks like from Run.
	cs := connectViaRun(t, &tgclient.Config{APIID: 1, APIHash: "hash"},
		staticConnect(false, errors.New("starting Telegram client: corrupted key")))

	tools, err := cs.ListTools(t.Context(), &mcp.ListToolsParams{})
	require.NoError(t, err)
	require.Len(t, tools.Tools, 1)
	assert.Equal(t, loginRequiredTool, tools.Tools[0].Name)
	assert.Contains(t, cs.InitializeResult().Instructions, "corrupted key",
		"the reason gotd reported has to reach the model")
}

// TestSummarizeMisconfigurationDisablesOnlySummarizeChat pins that an invalid
// summarisation setting — an optional feature — is logged at startup but
// blocks nothing: every tool is served, and SummarizeChat alone fails, with
// the precise startup error and the fix for the transport.
func TestSummarizeMisconfigurationDisablesOnlySummarizeChat(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	var logs bytes.Buffer
	tgClient := newFakeClient()
	srv, err := newServerConnecting(Options{
		Config: &tgclient.Config{APIID: 1, APIHash: "hash"},
		Summarize: summarize.Config{
			Provider:     summarize.ProviderGemini,
			BatchTokens:  1,
			GeminiAPIKey: func() (string, error) { return "", nil },
		},
		Version:   "test",
		Variant:   variantFull,
		Stdin:     serverR,
		Stdout:    serverW,
		ErrOut:    &logs,
		Transport: TransportStdio,
	}, func(context.Context) (localClient, error) { return tgClient, nil })
	require.NoError(t, err)
	assert.Contains(t, logs.String(), "level=WARN")
	assert.Contains(t, logs.String(), "the gemini API key is not set")

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(t.Context()) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).
		Connect(t.Context(), &mcp.IOTransport{Reader: clientR, Writer: clientW}, nil)
	require.NoError(t, err)

	listed, err := cs.ListTools(t.Context(), &mcp.ListToolsParams{})
	require.NoError(t, err)
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	assert.Contains(t, names, "SummarizeChat", "the tool stays registered to report the problem")
	assert.Contains(t, names, "GetMe")
	assert.NotContains(t, names, loginRequiredTool)
	assert.Equal(t, happyInstructions, cs.InitializeResult().Instructions)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "SummarizeChat", Arguments: map[string]any{"chat_id": 1, "goal": "recap"}})
	require.NoError(t, err)
	require.True(t, res.IsError)
	text := res.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, "Failed to summarise chat 1: summarisation is not available")
	assert.Contains(t, text, "the gemini API key is not set")
	assert.Contains(t, text, "MCP_SUMMARIZE_GEMINI_API_KEY")
	assert.Contains(t, text, "Reconnect")

	res, err = cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "GetMe", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, res.IsError, "every other tool works")
	assert.Contains(t, res.Content[0].(*mcp.TextContent).Text, fakeSelf)

	require.NoError(t, cs.Close())
	_ = clientW.Close()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the host disconnected")
	}
	assert.True(t, tgClient.isClosed(), "Run disconnects the client once the host leaves")

	assert.ErrorContains(t, summarizeUnavailable(TransportHTTP, errors.New("bad")), "restart the server")
}

// blockingSession parks LoadSession until the client's own context ends, so
// a client started on it never becomes ready.
type blockingSession struct{}

func (blockingSession) LoadSession(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingSession) StoreSession(context.Context, []byte) error { return nil }

// TestRunCancelledDuringStartupReturnsQuietly pins that a host shutting the
// server down while the Telegram client is still starting is not a failure:
// Run returns nil without an Error record and without detouring through
// login-required mode.
func TestRunCancelledDuringStartupReturnsQuietly(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	defer func() { _ = stdinW.Close() }()
	var logs bytes.Buffer
	started := make(chan struct{})
	srv, err := newServer(Options{
		Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
		Summarize: testSummarize,
		Version:   "test",
		Stdin:     stdinR,
		Stdout:    io.Discard,
		ErrOut:    &logs,
		Transport: TransportStdio,
	})
	require.NoError(t, err)
	srv.connectLocal = func(ctx context.Context) (localClient, error) {
		close(started)
		running, err := tgclient.StartClient(ctx, srv.opts.Config, blockingSession{}, srv.logger, srv.floodWaitLogger())
		if err != nil {
			return nil, err
		}
		return running, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()
	<-started
	cancel()

	select {
	case runErr := <-errCh:
		require.NoError(t, runErr)
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Run did not return within 3s after ctx cancel during startup")
	}
	assert.NotContains(t, logs.String(), "level=ERROR")
	assert.NotContains(t, logs.String(), "login-required")
}
