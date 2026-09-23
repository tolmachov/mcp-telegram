package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// fakeClient is a never-connected Telegram client that a test can stop with a
// reason, the way a *tgclient.Running stops on its own.
type fakeClient struct {
	api  *tg.Client
	done chan struct{}

	mu  sync.Mutex
	err error
}

func newFakeClient() *fakeClient {
	return &fakeClient{api: telegram.NewClient(1, "hash", telegram.Options{}).API(), done: make(chan struct{})}
}

func (c *fakeClient) API() *tg.Client       { return c.api }
func (c *fakeClient) Done() <-chan struct{} { return c.done }

func (c *fakeClient) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *fakeClient) stop(reason error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = reason
	close(c.done)
}

func TestBuildAssemblyForVariantModesWithoutTelegramConnection(t *testing.T) {
	for _, variant := range []string{"", variantFull, variantCompact, variantResearch} {
		t.Run(variant, func(t *testing.T) {
			srv, err := New(Options{
				Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
				Summarize: testSummarize,
				Version:   "test",
				Variant:   variant,
				Stdin:     strings.NewReader(""),
				Stdout:    io.Discard,
				ErrOut:    io.Discard,
				Transport: TransportStdio,
			})
			require.NoError(t, err)
			assembly, err := srv.buildAssembly(t.Context(), newFakeClient(), testLogger())
			require.NoError(t, err)
			if variant == "" {
				require.NotNil(t, assembly.variants)
			} else {
				require.NotNil(t, assembly.single)
			}
			require.NoError(t, assembly.Close())
			select {
			case <-assembly.watchDone:
			default:
				t.Fatal("Close must not return before the pinned-chat watcher has exited")
			}
		})
	}
}

func TestServeAssemblyPinnedVariantExitsOnStdinEOF(t *testing.T) {
	srv, err := New(Options{
		Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
		Summarize: testSummarize,
		Version:   "test",
		Variant:   variantResearch,
		Stdin:     strings.NewReader(""),
		Stdout:    io.Discard,
		ErrOut:    io.Discard,
		Transport: TransportStdio,
	})
	require.NoError(t, err)
	require.NoError(t, srv.serveAssembly(t.Context(), newFakeClient()))
}

func TestServeAssemblyAllVariantsExitsOnStdinEOF(t *testing.T) {
	srv, err := New(Options{
		Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
		Summarize: testSummarize,
		Version:   "test",
		Stdin:     strings.NewReader(""),
		Stdout:    io.Discard,
		ErrOut:    io.Discard,
		Transport: TransportStdio,
	})
	require.NoError(t, err)
	require.NoError(t, srv.serveAssembly(t.Context(), newFakeClient()))
}

// TestStdioRefusedSessionEntersLoginRequiredState pins the stdio half of a
// session Telegram refuses mid-run: the host keeps its connection, and every
// tool call and resource read answers with the login-required reason instead
// of reaching Telegram.
func TestStdioRefusedSessionEntersLoginRequiredState(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	srv, err := New(Options{
		Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
		Summarize: testSummarize,
		Version:   "test",
		Variant:   variantFull,
		Stdin:     serverR,
		Stdout:    serverW,
		ErrOut:    io.Discard,
		Transport: TransportStdio,
	})
	require.NoError(t, err)

	tgClient := newFakeClient()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.serveAssembly(t.Context(), tgClient) }()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).
		Connect(t.Context(), &mcp.IOTransport{Reader: clientR, Writer: clientW}, nil)
	require.NoError(t, err)

	tgClient.stop(fmt.Errorf("%w: %w", tgclient.ErrSessionUnauthorized, tgerr.New(401, "SESSION_REVOKED")))

	for range 2 {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "GetMe", Arguments: map[string]any{}})
		require.NoError(t, err, "the session stays up")
		require.True(t, res.IsError)
		text := res.Content[0].(*mcp.TextContent).Text
		assert.Contains(t, text, "SESSION_REVOKED")
		assert.Contains(t, text, notLoggedInMessage)
	}
	_, err = cs.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "telegram://me"})
	require.ErrorContains(t, err, notLoggedInMessage)

	require.NoError(t, cs.Close())
	_ = clientW.Close()
	select {
	case err := <-errCh:
		require.NoError(t, err, "a host disconnect after the session died is a normal shutdown")
	case <-time.After(3 * time.Second):
		t.Fatal("serving did not stop after the host disconnected")
	}
}

// TestClientDownMiddlewareAnswersForAStoppedClient pins both halves of the
// middleware: a call that fails because the client stopped under it keeps its
// own outcome with the transport's answer appended, a call that succeeded
// anyway keeps its result, and later calls never reach the handler.
func TestClientDownMiddlewareAnswersForAStoppedClient(t *testing.T) {
	refused := fmt.Errorf("%w: %w", tgclient.ErrSessionUnauthorized, tgerr.New(401, "AUTH_KEY_UNREGISTERED"))
	for _, transport := range []string{TransportStdio, TransportHTTP} {
		srv := &Server{opts: Options{Transport: transport}}
		want := srv.clientDownText(refused)
		call := &mcp.CallToolRequest{}

		// A backup that stopped part-way reports the file it saved; that
		// outcome must survive the client going down under it.
		const backupFailure = "Failed to back up chat 5: fetching batch 2: telegram session is not authorized. The 200 messages fetched before the failure were saved to /tmp/backup.json."
		tgClient := newFakeClient()
		failing := srv.clientDownMiddleware(tgClient)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
			tgClient.stop(refused)
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: backupFailure}}}, nil
		})
		res, err := failing(t.Context(), methodCallTool, call)
		require.NoError(t, err)
		tr := res.(*mcp.CallToolResult)
		assert.True(t, tr.IsError)
		require.Len(t, tr.Content, 2, transport)
		assert.Equal(t, backupFailure, tr.Content[0].(*mcp.TextContent).Text, "the call's own outcome is kept")
		assert.Equal(t, want, tr.Content[1].(*mcp.TextContent).Text, transport)

		tgClient = newFakeClient()
		readErr := errors.New("reading telegram://me: telegram session is not authorized")
		failingRead := srv.clientDownMiddleware(tgClient)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
			tgClient.stop(refused)
			return nil, readErr
		})
		_, err = failingRead(t.Context(), methodReadResource, &mcp.ReadResourceRequest{})
		require.ErrorIs(t, err, readErr, "the read's own error is kept")
		assert.ErrorContains(t, err, want)

		tgClient = newFakeClient()
		succeeded := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}
		racing := srv.clientDownMiddleware(tgClient)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
			tgClient.stop(refused)
			return succeeded, nil
		})
		res, err = racing(t.Context(), methodCallTool, call)
		require.NoError(t, err)
		assert.Same(t, succeeded, res, "a call that succeeded keeps its result")

		unreachable := srv.clientDownMiddleware(tgClient)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
			t.Fatal("a call after the client stopped must not reach the handler")
			return nil, nil
		})
		res, err = unreachable(t.Context(), methodCallTool, call)
		require.NoError(t, err)
		tr = res.(*mcp.CallToolResult)
		require.Len(t, tr.Content, 1, "a call that never ran has only the client-down answer")
		assert.Equal(t, want, tr.Content[0].(*mcp.TextContent).Text, transport)
	}
}

// TestClientDownTextFitsTheTransport pins that the answer names the recovery
// the transport actually offers.
func TestClientDownTextFitsTheTransport(t *testing.T) {
	refused := fmt.Errorf("%w: %w", tgclient.ErrSessionUnauthorized, tgerr.New(401, "SESSION_REVOKED"))
	dropped := errors.New("telegram client stopped: key fingerprint not found")
	stdio := &Server{opts: Options{Transport: TransportStdio}}
	httpSrv := &Server{opts: Options{Transport: TransportHTTP}}

	assert.Contains(t, stdio.clientDownText(refused), notLoggedInMessage)
	assert.Contains(t, stdio.clientDownText(dropped), "reconnected")
	assert.NotContains(t, stdio.clientDownText(dropped), "not logged in")
	assert.Contains(t, httpSrv.clientDownText(refused), "QR login")
	assert.NotContains(t, httpSrv.clientDownText(refused), "mcp-telegram login")
	assert.Contains(t, httpSrv.clientDownText(dropped), "Retry")
	assert.NotContains(t, httpSrv.clientDownText(dropped), "QR login")
}

// TestServeAssemblyTreatsHostCancelAsShutdown covers SIGINT: a cancelled ctx
// must not exit non-zero.
func TestServeAssemblyTreatsHostCancelAsShutdown(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	defer func() { _ = stdinW.Close() }()
	srv, err := New(Options{
		Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
		Summarize: testSummarize,
		Version:   "test",
		Variant:   variantFull,
		Stdin:     stdinR,
		Stdout:    io.Discard,
		ErrOut:    io.Discard,
		Transport: TransportStdio,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.serveAssembly(ctx, newFakeClient()) }()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("serving did not stop after ctx cancel")
	}
}

func TestStreamableHTTPOptionsCarrySessionTimeout(t *testing.T) {
	opts := streamableHTTPOptions()
	require.NotNil(t, opts)
	assert.Equal(t, mcpSessionTimeout, opts.SessionTimeout)
}

func TestServerAuxiliaryLifecycleBranches(t *testing.T) {
	srv, err := New(Options{
		Config:    &tgclient.Config{APIID: 1, APIHash: "hash", FloodWaitMaxWait: 2 * time.Second},
		Summarize: testSummarize,
		Version:   "test",
		Stdin:     strings.NewReader(""),
		Stdout:    io.Discard,
		ErrOut:    io.Discard,
		Transport: TransportStdio,
	})
	require.NoError(t, err)
	logFloodWait := srv.floodWaitLogger()
	logFloodWait(t.Context(), time.Second)
	logFloodWait(t.Context(), 3*time.Second)

	_, err = (&Server{opts: Options{Config: &tgclient.Config{}}}).startLogin(t.Context())
	require.Error(t, err)

	srv.opts.Variant = "unknown"
	_, err = srv.buildAssembly(t.Context(), newFakeClient(), testLogger())
	require.ErrorContains(t, err, "variant")

	first := errors.New("first")
	second := errors.New("second")
	closed := 0
	err = (multiCloser{
		closerFunc(func() error { closed++; return first }),
		closerFunc(func() error { closed++; return second }),
	}).Close()
	assert.Equal(t, 2, closed)
	assert.ErrorIs(t, err, first)
	assert.ErrorIs(t, err, second)
}

func TestLoginRequiredSmallHelpers(t *testing.T) {
	assert.Empty(t, accountSuffix(""))
	assert.Equal(t, " as alice", accountSuffix("alice"))
	blocked := &Server{opts: Options{Transport: TransportHTTP}}
	assert.ErrorContains(t, blocked.startBlocked(t.Context(), "blocked reason"), "blocked reason")
}

func TestSafeValueCoversScalarAndTruncation(t *testing.T) {
	assert.Equal(t, "short", safeValue(json.RawMessage(`"short"`)))
	assert.Equal(t, "42", safeValue(json.RawMessage(`42`)))
	long := strings.Repeat("x", maxParamValueLen+1)
	assert.Contains(t, safeValue(json.RawMessage(`"`+long+`"`)), "truncated")
}
