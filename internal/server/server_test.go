package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

func newTestLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

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

// newServerWithPipes builds a Server wired up to an io.Pipe for stdin and a
// bytes.Buffer for stdout. The returned stdinW lets tests feed JSON-RPC
// frames to the server; stdout captures what the server writes back.
func newServerWithPipes(t *testing.T, cfg *tgclient.Config) (*Server, *io.PipeWriter, *bytes.Buffer) {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdout := &bytes.Buffer{}
	errOut := &bytes.Buffer{}

	srv, err := New(Options{
		Config:  cfg,
		Version: "test",
		Stdin:   stdinR,
		Stdout:  stdout,
		ErrOut:  errOut,
	})
	require.NoError(t, err)
	return srv, stdinW, stdout
}

// decodeInitErrorFrame parses the JSON-RPC error response that
// replyInitError/writeInitErrorResponse emits on stdout.
func decodeInitErrorFrame(t *testing.T, raw []byte) (id json.RawMessage, code int, message string) {
	t.Helper()
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(raw), &resp))
	assert.Equal(t, "2.0", resp.JSONRPC)
	return resp.ID, resp.Error.Code, resp.Error.Message
}

func TestRunMissingCredentialsWritesInitError(t *testing.T) {
	srv, stdinW, stdout := newServerWithPipes(t, &tgclient.Config{})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	// Send a real initialize frame and assert the server answers with a
	// JSON-RPC error echoing the id.
	_, err := stdinW.Write([]byte(`{"jsonrpc":"2.0","id":42,"method":"initialize","params":{}}` + "\n"))
	require.NoError(t, err)

	select {
	case runErr := <-errCh:
		require.Error(t, runErr)
		assert.Contains(t, runErr.Error(), "refused to start")
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Run did not return within 3s after initialize")
	}
	_ = stdinW.Close()

	id, code, msg := decodeInitErrorFrame(t, stdout.Bytes())
	assert.JSONEq(t, `42`, string(id))
	assert.Equal(t, initErrorCode, code)
	assert.Contains(t, msg, "TELEGRAM_API_ID")
	assert.Contains(t, msg, "TELEGRAM_API_HASH")
}

func TestRunMissingCredentialsEchoesStringID(t *testing.T) {
	srv, stdinW, stdout := newServerWithPipes(t, &tgclient.Config{})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	_, err := stdinW.Write([]byte(`{"jsonrpc":"2.0","id":"req-abc","method":"initialize","params":{}}` + "\n"))
	require.NoError(t, err)

	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Run did not return within 3s")
	}
	_ = stdinW.Close()

	id, code, _ := decodeInitErrorFrame(t, stdout.Bytes())
	assert.JSONEq(t, `"req-abc"`, string(id))
	assert.Equal(t, initErrorCode, code)
}

func TestRunSkipsNonInitializeFrames(t *testing.T) {
	srv, stdinW, stdout := newServerWithPipes(t, &tgclient.Config{})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	// Junk before the real initialize request — server must be lenient and
	// still produce the error response for the eventual initialize.
	_, err := stdinW.Write([]byte(
		"not-json-at-all\n" +
			`{"jsonrpc":"2.0","method":"notifications/cancelled"}` + "\n" +
			`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{}}` + "\n",
	))
	require.NoError(t, err)

	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Run did not return within 3s")
	}
	_ = stdinW.Close()

	// Only one JSON-RPC frame should have been written: the one for id=7.
	lines := bytes.Split(bytes.TrimRight(stdout.Bytes(), "\n"), []byte("\n"))
	require.Len(t, lines, 1, "expected exactly one response frame, got %d", len(lines))
	id, _, _ := decodeInitErrorFrame(t, lines[0])
	assert.JSONEq(t, `7`, string(id))
}

func TestRunEOFBeforeInitialize(t *testing.T) {
	srv, stdinW, stdout := newServerWithPipes(t, &tgclient.Config{})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	// Close stdin without sending any frame — server should surface an
	// ErrUnexpectedEOF-shaped error and not hang.
	require.NoError(t, stdinW.Close())

	select {
	case runErr := <-errCh:
		require.Error(t, runErr)
		assert.True(t,
			errors.Is(runErr, io.ErrUnexpectedEOF) || strings.Contains(runErr.Error(), "unexpected EOF"),
			"expected ErrUnexpectedEOF, got %v", runErr,
		)
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Run did not return within 3s on stdin close")
	}

	// No JSON-RPC frame should have been written — there was no id to echo.
	assert.Equal(t, 0, stdout.Len(), "stdout should be empty when no initialize arrived, got %q", stdout.String())
}

func TestRunContextCancelBeforeInitialize(t *testing.T) {
	srv, stdinW, _ := newServerWithPipes(t, &tgclient.Config{})
	defer func() { _ = stdinW.Close() }()

	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	// Give the goroutine a tick to start blocking on stdin.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case runErr := <-errCh:
		require.Error(t, runErr)
		assert.True(t,
			errors.Is(runErr, context.Canceled) || strings.Contains(runErr.Error(), "context canceled"),
			"expected context.Canceled, got %v", runErr,
		)
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Run did not return within 3s after ctx cancel")
	}
}

func TestIsTTYRejectsNonFile(t *testing.T) {
	// bytes.Buffer is not *os.File — must be treated as non-TTY so the
	// init-error flow writes a JSON-RPC frame instead of returning a plain
	// Go error.
	assert.False(t, isTTY(&bytes.Buffer{}))
}

func TestIsTTYRejectsPipe(t *testing.T) {
	// os.Pipe returns *os.File but pipes are not character devices, so
	// isTTY must return false and take the JSON-RPC response path.
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()
	assert.False(t, isTTY(r))
}

func TestWriteInitErrorResponseShape(t *testing.T) {
	// Direct test of the frame writer: id echoed verbatim, error code fixed
	// at initErrorCode, trailing newline present.
	var out bytes.Buffer
	srv := &Server{
		logger: newTestLogger(&out),
		stdout: &out,
	}

	err := srv.writeInitErrorResponse(json.RawMessage(`"xyz"`), "boom")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refused to start")
	assert.Contains(t, err.Error(), "boom")

	// Must be exactly one line with a trailing newline.
	b := out.Bytes()
	require.True(t, len(b) > 0 && b[len(b)-1] == '\n', "expected trailing newline")

	id, code, message := decodeInitErrorFrame(t, b)
	assert.JSONEq(t, `"xyz"`, string(id))
	assert.Equal(t, initErrorCode, code)
	assert.Equal(t, "boom", message)
}

// shortWriter returns n<len(p) on Write with err==nil on the first call to
// exercise writeInitErrorResponse's fully-written loop.
type shortWriter struct {
	buf   bytes.Buffer
	first bool
}

func (s *shortWriter) Write(p []byte) (int, error) {
	if !s.first {
		s.first = true
		if len(p) > 1 {
			return s.buf.Write(p[:1])
		}
	}
	return s.buf.Write(p)
}

func TestWriteInitErrorResponseHandlesShortWrite(t *testing.T) {
	sw := &shortWriter{}
	srv := &Server{
		logger: newTestLogger(io.Discard),
		stdout: sw,
	}
	err := srv.writeInitErrorResponse(json.RawMessage(`1`), "hello")
	require.Error(t, err)

	// Even with a short write on the first call, the full frame must land.
	id, code, message := decodeInitErrorFrame(t, sw.buf.Bytes())
	assert.JSONEq(t, `1`, string(id))
	assert.Equal(t, initErrorCode, code)
	assert.Equal(t, "hello", message)
}

// errWriter always fails with a fixed error. Exercises the write-error
// branch of writeInitErrorResponse.
type errWriter struct{ err error }

func (e errWriter) Write([]byte) (int, error) { return 0, e.err }

func TestWriteInitErrorResponseSurfacesWriterError(t *testing.T) {
	wantErr := errors.New("broken pipe")
	srv := &Server{
		logger: newTestLogger(io.Discard),
		stdout: errWriter{err: wantErr},
	}
	err := srv.writeInitErrorResponse(json.RawMessage(`1`), "hello")
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	// A writer failure must not masquerade as the "refused to start"
	// success-after-frame-sent return value.
	assert.NotContains(t, err.Error(), "refused to start")
}

// stuckWriter always returns (0, nil), violating the io.Writer spirit but
// allowed by its letter. Exercises the no-progress guard.
type stuckWriter struct{}

func (stuckWriter) Write(p []byte) (int, error) { return 0, nil }

func TestWriteInitErrorResponseRejectsZeroProgressWriter(t *testing.T) {
	srv := &Server{
		logger: newTestLogger(io.Discard),
		stdout: stuckWriter{},
	}
	err := srv.writeInitErrorResponse(json.RawMessage(`1`), "hello")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no progress")
}
