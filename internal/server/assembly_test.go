package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

func disconnectedTelegramAPI() *tg.Client {
	return telegram.NewClient(1, "hash", telegram.Options{}).API()
}

func TestBuildAssemblyForVariantModesWithoutTelegramConnection(t *testing.T) {
	for _, variant := range []string{"", variantFull, variantCompact, variantResearch} {
		t.Run(variant, func(t *testing.T) {
			srv, err := New(Options{
				Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
				Version:   "test",
				Variant:   variant,
				Stdin:     strings.NewReader(""),
				Stdout:    io.Discard,
				ErrOut:    io.Discard,
				Transport: TransportStdio,
			})
			require.NoError(t, err)
			assembly, err := srv.buildAssembly(t.Context(), disconnectedTelegramAPI(), testLogger())
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
		Version:   "test",
		Variant:   variantResearch,
		Stdin:     strings.NewReader(""),
		Stdout:    io.Discard,
		ErrOut:    io.Discard,
		Transport: TransportStdio,
	})
	require.NoError(t, err)
	require.NoError(t, srv.serveAssembly(t.Context(), disconnectedTelegramAPI(), make(chan struct{})))
}

func TestServeAssemblyAllVariantsExitsOnStdinEOF(t *testing.T) {
	srv, err := New(Options{
		Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
		Version:   "test",
		Stdin:     strings.NewReader(""),
		Stdout:    io.Discard,
		ErrOut:    io.Discard,
		Transport: TransportStdio,
	})
	require.NoError(t, err)
	require.NoError(t, srv.serveAssembly(t.Context(), disconnectedTelegramAPI(), make(chan struct{})))
}

// TestServeAssemblyStopsWhenTheClientDoes pins the phase rule: a client that
// stops after serving began ends the serve loop and is reported as a serve
// failure, never re-diagnosed as a blocked startup.
func TestServeAssemblyStopsWhenTheClientDoes(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	defer func() { _ = stdinW.Close() }()
	srv, err := New(Options{
		Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
		Version:   "test",
		Variant:   variantFull,
		Stdin:     stdinR,
		Stdout:    io.Discard,
		ErrOut:    io.Discard,
		Transport: TransportStdio,
	})
	require.NoError(t, err)

	clientDone := make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.serveAssembly(t.Context(), disconnectedTelegramAPI(), clientDone) }()
	close(clientDone)

	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "running MCP server")
	case <-time.After(3 * time.Second):
		t.Fatal("serving did not stop after the Telegram client stopped")
	}
}

// TestServeAssemblyTreatsHostCancelAsShutdown covers SIGINT: a cancelled ctx
// must not exit non-zero.
func TestServeAssemblyTreatsHostCancelAsShutdown(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	defer func() { _ = stdinW.Close() }()
	srv, err := New(Options{
		Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
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
	go func() { errCh <- srv.serveAssembly(ctx, disconnectedTelegramAPI(), make(chan struct{})) }()
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
	_, err = srv.buildAssembly(t.Context(), disconnectedTelegramAPI(), testLogger())
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
