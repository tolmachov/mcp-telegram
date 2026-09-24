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

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// fakeClient is a never-connected Telegram client that a test can stop with a
// reason, the way a *tgclient.Running stops on its own. Its API answers
// users.getFullUser for the self user and parks every other request until
// the request's context ends.
type fakeClient struct {
	api  *tg.Client
	done chan struct{}

	mu     sync.Mutex
	err    error
	closed bool
}

func newFakeClient() *fakeClient {
	return &fakeClient{api: tg.NewClient(fakeInvoker{}), done: make(chan struct{})}
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
	if c.err == nil {
		c.err = reason
		close(c.done)
	}
}

func (c *fakeClient) Close() {
	c.stop(errors.New("client closed"))
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}

func (c *fakeClient) Self() *tg.User { return &tg.User{ID: 42, Self: true, FirstName: fakeSelf} }

func (c *fakeClient) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// fakeSelf is the account fakeClient is logged in as.
const fakeSelf = "Ada"

type fakeInvoker struct{}

func (fakeInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	if _, ok := input.(*tg.UsersGetFullUserRequest); ok {
		*output.(*tg.UsersUserFull) = tg.UsersUserFull{Users: []tg.UserClass{&tg.User{ID: 42, Self: true, FirstName: fakeSelf}}}
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
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
			client := newFakeClient()
			assembly, err := srv.buildAssembly(t.Context(), client, testLogger())
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
			assert.True(t, client.isClosed(), "the assembly owns its client: Close disconnects it")
		})
	}
}

// TestClientStopEndsAssemblyLifetime pins that a client stopping on its own
// ends the assembly's lifetime: the pinned-chat watcher, parked in a Telegram
// call on it, exits without the assembly being closed.
func TestClientStopEndsAssemblyLifetime(t *testing.T) {
	srv, err := New(Options{
		Config:        &tgclient.Config{APIID: 1, APIHash: "hash"},
		Summarize:     testSummarize,
		Version:       "test",
		Variant:       variantFull,
		PinnedRefresh: time.Hour,
		Stdin:         strings.NewReader(""),
		Stdout:        io.Discard,
		ErrOut:        io.Discard,
		Transport:     TransportStdio,
	})
	require.NoError(t, err)
	client := newFakeClient()
	asm, err := srv.buildAssembly(t.Context(), client, testLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = asm.Close() })

	select {
	case <-asm.watchDone:
		t.Fatal("the watcher exited while the client still serves")
	default:
	}
	client.stop(errors.New("read tcp: connection reset by peer"))
	select {
	case <-asm.watchDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the client stopped but the assembly's lifetime did not end")
	}
	assert.False(t, client.isClosed(), "a client stopping on its own is not closed by the assembly until Close")
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
// middleware: a call the client stopped under keeps its own outcome, failed
// or not, with the transport's answer appended — the one for a completed
// call when it succeeded — and later calls never reach the handler.
func TestClientDownMiddlewareAnswersForAStoppedClient(t *testing.T) {
	stops := map[string]error{
		"refused": fmt.Errorf("%w: %w", tgclient.ErrSessionUnauthorized, tgerr.New(401, "AUTH_KEY_UNREGISTERED")),
		"dropped": errors.New("telegram client stopped: key fingerprint not found"),
	}
	for _, transport := range []string{TransportStdio, TransportHTTP} {
		for name, stop := range stops {
			t.Run(transport+"/"+name, func(t *testing.T) { testClientDownAnswers(t, transport, stop) })
		}
	}
}

// testClientDownAnswers is one transport and stop reason of
// TestClientDownMiddlewareAnswersForAStoppedClient.
func testClientDownAnswers(t *testing.T, transport string, stop error) {
	srv := &Server{opts: Options{Transport: transport}}
	want := srv.clientDownText(stop, false)
	call := &mcp.CallToolRequest{}

	// A backup that stopped part-way reports the file it saved; that
	// outcome must survive the client going down under it.
	const backupFailure = "Failed to back up chat 5: fetching batch 2: engine was closed. The 200 messages fetched before the failure were saved to /tmp/backup.json."
	tgClient := newFakeClient()
	failing := srv.clientDownMiddleware(tgClient)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		tgClient.stop(stop)
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
		tgClient.stop(stop)
		return nil, readErr
	})
	_, err = failingRead(t.Context(), methodReadResource, &mcp.ReadResourceRequest{})
	require.ErrorIs(t, err, readErr, "the read's own error is kept")
	assert.ErrorContains(t, err, want)

	// A batch the stop cut short returns what it managed, with a
	// warning; the answer for a completed call is appended to it, so
	// the model does not repeat what it did. The handler's own Content
	// has room to grow, which the middleware must not write into.
	tgClient = newFakeClient()
	var backing [4]mcp.Content
	backing[0] = &mcp.TextContent{Text: `{"successful":2,"warning":"context canceled."}`}
	handlerContent := backing[:1]
	cutShort := &mcp.CallToolResult{Content: handlerContent, StructuredContent: map[string]any{"successful": 2}}
	racing := srv.clientDownMiddleware(tgClient)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		tgClient.stop(stop)
		return cutShort, nil
	})
	res, err = racing(t.Context(), methodCallTool, call)
	require.NoError(t, err)
	tr = res.(*mcp.CallToolResult)
	assert.False(t, tr.IsError, "a call that returned what it managed stays a success")
	assert.Equal(t, cutShort.StructuredContent, tr.StructuredContent)
	require.Len(t, tr.Content, 2, transport)
	assert.Same(t, handlerContent[0], tr.Content[0], "the call's own outcome is kept")
	completed := tr.Content[1].(*mcp.TextContent).Text
	assert.Equal(t, srv.clientDownText(stop, true), completed, transport)
	assert.NotContains(t, completed, "Retry", "a call that completed is never to be repeated")
	assert.Len(t, cutShort.Content, 1, "the handler's result is not changed")
	assert.Nil(t, backing[1], "nor is its Content's spare capacity written")

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

// TestClientDownTextFitsTheTransport pins that the answer names the recovery
// the transport actually offers.
func TestClientDownTextFitsTheTransport(t *testing.T) {
	refused := fmt.Errorf("%w: %w", tgclient.ErrSessionUnauthorized, tgerr.New(401, "SESSION_REVOKED"))
	dropped := errors.New("telegram client stopped: key fingerprint not found")
	secondary := fmt.Errorf("%w (%w) while the home DC still accepts it; reconnecting restores the calls it serves", tgclient.ErrSecondaryRefusal, tgerr.New(401, "AUTH_KEY_UNREGISTERED"))
	stdio := &Server{opts: Options{Transport: TransportStdio}}
	httpSrv := &Server{opts: Options{Transport: TransportHTTP}}

	for _, completed := range []bool{false, true} {
		assert.Contains(t, stdio.clientDownText(refused, completed), notLoggedInMessage)
		assert.Contains(t, stdio.clientDownText(dropped, completed), "reconnected")
		assert.NotContains(t, stdio.clientDownText(dropped, completed), "not logged in")
		assert.Contains(t, stdio.clientDownText(secondary, completed), "reconnected")
		assert.NotContains(t, stdio.clientDownText(secondary, completed), "not logged in", "the home DC still accepts the session")
		assert.Contains(t, httpSrv.clientDownText(refused, completed), "QR login")
		assert.NotContains(t, httpSrv.clientDownText(refused, completed), "mcp-telegram login")
		assert.NotContains(t, httpSrv.clientDownText(dropped, completed), "QR login")
		assert.NotContains(t, httpSrv.clientDownText(secondary, completed), "QR login", "the home DC still accepts the session")
		assert.Contains(t, httpSrv.clientDownText(secondary, completed), "request reconnects it")
	}
	assert.Contains(t, httpSrv.clientDownText(dropped, false), "Retry")

	// A call that completed is told its result stands, and is never asked to
	// run again.
	for _, srv := range []*Server{stdio, httpSrv} {
		for _, err := range []error{refused, dropped, secondary} {
			text := srv.clientDownText(err, true)
			assert.True(t, strings.HasPrefix(text, "This call completed before the Telegram connection stopped: what its result reports stands, so do not repeat it."), text)
			assert.NotContains(t, text, "Retry", text)
			assert.NotContains(t, srv.clientDownText(err, false), "completed", "a call that failed or never ran did not complete")
		}
	}
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
	client := newFakeClient()
	_, err = srv.buildAssembly(t.Context(), client, testLogger())
	require.ErrorContains(t, err, "variant")
	assert.True(t, client.isClosed(), "a failed build disconnects the client it was handed")
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
