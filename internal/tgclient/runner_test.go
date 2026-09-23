package tgclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/gotd/log"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsSessionUnauthorizedSeparatesRejectionFromUnreachable guards the distinction
// the re-check depends on. Telegram rejecting the stored key is a verdict
// ("log in again"); anything else is an undetermined check ("retry"). Getting
// this wrong is not cosmetic: a revoked session fails in the connect phase, so
// the callback never runs and there is no Status to read — without this test
// the tool reports check_failed and advises the user to check their network
// for a session they revoked themselves.
func TestIsSessionUnauthorizedSeparatesRejectionFromUnreachable(t *testing.T) {
	dead := []error{
		tgerr.New(401, "AUTH_KEY_UNREGISTERED"),
		tgerr.New(406, "SESSION_EXPIRED"),
		tgerr.New(406, "AUTH_KEY_DUPLICATED"),
		tgerr.New(401, "SESSION_REVOKED"),
		// Must survive the wrapping gotd applies on the way out.
		fmt.Errorf("callback: %w", tgerr.New(401, "AUTH_KEY_UNREGISTERED")),
	}
	for _, err := range dead {
		assert.True(t, IsSessionUnauthorized(err), "expected a dead-session verdict for %v", err)
	}

	alive := []error{
		errors.New("dial tcp: no route to host"),
		context.DeadlineExceeded,
		tgerr.New(420, "FLOOD_WAIT_30"),
		tgerr.New(500, "INTERNAL_SERVER_ERROR"),
	}
	for _, err := range alive {
		assert.False(t, IsSessionUnauthorized(err), "expected an undetermined check for %v", err)
	}
}

func TestRunningPreservesCauseAndClassifiesSessionErrors(t *testing.T) {
	for _, original := range []error{ErrSessionUnauthorized, tgerr.New(401, "AUTH_KEY_UNREGISTERED"), tgerr.New(406, "SESSION_EXPIRED"), errors.New("storage unavailable"), context.Canceled} {
		r := &Running{cancel: func() {}}
		r.stop(sessionError(original))
		require.ErrorIs(t, r.Err(), original)
		assert.Equal(t, IsSessionUnauthorized(original), errors.Is(r.Err(), ErrSessionUnauthorized))
	}
}

// failingInvoker answers every call with err.
type failingInvoker struct{ err error }

func (i failingInvoker) Invoke(context.Context, bin.Encoder, bin.Decoder) error { return i.err }

// TestRefusalWatchStopsTheClient pins the client-layer detection: the first
// call Telegram answers with a dead-session error stops the client at once —
// Err reports it before the call has even returned to its caller — while any
// other failure leaves the client serving.
func TestRefusalWatchStopsTheClient(t *testing.T) {
	newRunning := func() (*Running, context.Context) {
		ctx, cancel := context.WithCancel(t.Context())
		return &Running{cancel: cancel}, ctx
	}
	call := func(r *Running, err error) error {
		return refusalWatch(r.refused).Handle(failingInvoker{err: err}).Invoke(t.Context(), &tg.UsersGetUsersRequest{}, &tg.UserClassVector{})
	}

	for _, alive := range []error{errors.New("dial tcp: no route to host"), tgerr.New(420, "FLOOD_WAIT_30"), tgerr.New(400, "CHANNEL_INVALID")} {
		r, ctx := newRunning()
		require.ErrorIs(t, call(r, alive), alive)
		require.NoError(t, r.Err(), "%v must not stop the client", alive)
		require.NoError(t, ctx.Err())
	}

	r, ctx := newRunning()
	refusal := tgerr.New(401, "SESSION_REVOKED")
	require.ErrorIs(t, call(r, refusal), refusal, "the caller still sees Telegram's own error")
	require.ErrorIs(t, r.Err(), ErrSessionUnauthorized)
	require.ErrorIs(t, r.Err(), refusal)
	require.Error(t, ctx.Err(), "the Run loop is told to stop")

	// A later reason never overwrites the refusal.
	r.stop(errClosed)
	require.ErrorIs(t, r.Err(), ErrSessionUnauthorized)
}

// TestStartTimeoutErrorCarriesTheConnectionFailure pins that a client which
// never becomes ready reports why: the last connection failure gotd reported
// while it kept retrying, or that none was reported.
func TestStartTimeoutErrorCarriesTheConnectionFailure(t *testing.T) {
	r := &Running{}
	assert.ErrorContains(t, r.startTimeoutError(), "no connection failure was reported")

	dial := errors.New("dial tcp 149.154.167.51:443: i/o timeout")
	r.connDead(errors.New("earlier failure"))
	r.connDead(dial)
	err := r.startTimeoutError()
	require.ErrorIs(t, err, dial)
	assert.ErrorContains(t, err, startClientTimeout.String())
}

// TestGotdLoggerKeepsWarningsOnly pins gotd's log floor: its per-connection
// Info chatter is dropped, its warnings reach the server's handler.
func TestGotdLoggerKeepsWarningsOnly(t *testing.T) {
	var buf bytes.Buffer
	logger := gotdLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	logger.Log(t.Context(), log.LevelInfo, "Starting")
	logger.Log(t.Context(), log.LevelWarn, "Permanent connection error, will not reconnect")
	assert.NotContains(t, buf.String(), "Starting")
	assert.Contains(t, buf.String(), "Permanent connection error")
	assert.Contains(t, buf.String(), "subsystem=gotd")
}

// loadFailingStorage fails LoadSession, which gotd reads in Run before it
// connects — the "callback never runs" route out of StartClient.
type loadFailingStorage struct{ err error }

func (s loadFailingStorage) LoadSession(context.Context) ([]byte, error) { return nil, s.err }
func (s loadFailingStorage) StoreSession(context.Context, []byte) error  { return nil }

// blockingStorage parks LoadSession until the client's own context ends.
type blockingStorage struct{}

func (blockingStorage) LoadSession(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingStorage) StoreSession(context.Context, []byte) error { return nil }

// TestStartClientClassifiesFailuresBeforeTheCallback pins that a session
// Telegram rejects before the ready callback runs still comes back as
// ErrSessionUnauthorized, while any other early failure keeps its cause and
// is not a verdict.
func TestStartClientClassifiesFailuresBeforeTheCallback(t *testing.T) {
	cfg := &Config{APIID: 1, APIHash: "hash"}

	_, err := StartClient(t.Context(), cfg, loadFailingStorage{err: tgerr.New(401, "AUTH_KEY_UNREGISTERED")}, discardLogger(), nil)
	require.ErrorIs(t, err, ErrSessionUnauthorized)

	storageErr := errors.New("keychain access denied")
	_, err = StartClient(t.Context(), cfg, loadFailingStorage{err: storageErr}, discardLogger(), nil)
	require.ErrorIs(t, err, storageErr)
	assert.NotErrorIs(t, err, ErrSessionUnauthorized)
}

// TestStartClientCancelledIsNotAVerdict pins that a startup cut short by its
// caller reports the cancellation, never a dead session.
func TestStartClientCancelledIsNotAVerdict(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := StartClient(ctx, &Config{APIID: 1, APIHash: "hash"}, blockingStorage{}, discardLogger(), nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrSessionUnauthorized)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
