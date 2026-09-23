package tgclient

import (
	"context"
	"errors"
	"fmt"
	"testing"

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
	for _, original := range []error{nil, ErrSessionUnauthorized, tgerr.New(401, "AUTH_KEY_UNREGISTERED"), tgerr.New(406, "SESSION_EXPIRED"), errors.New("storage unavailable"), context.Canceled} {
		r := &Running{}
		r.setRunErr(original)
		require.ErrorIs(t, r.RunErr(), original)
		assert.Equal(t, IsSessionUnauthorized(original), errors.Is(r.RunErr(), ErrSessionUnauthorized))
	}
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

	_, err := StartClient(t.Context(), cfg, loadFailingStorage{err: tgerr.New(401, "AUTH_KEY_UNREGISTERED")}, nil)
	require.ErrorIs(t, err, ErrSessionUnauthorized)

	storageErr := errors.New("keychain access denied")
	_, err = StartClient(t.Context(), cfg, loadFailingStorage{err: storageErr}, nil)
	require.ErrorIs(t, err, storageErr)
	assert.NotErrorIs(t, err, ErrSessionUnauthorized)
}

// TestStartClientCancelledIsNotAVerdict pins that a startup cut short by its
// caller reports the cancellation, never a dead session.
func TestStartClientCancelledIsNotAVerdict(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := StartClient(ctx, &Config{APIID: 1, APIHash: "hash"}, blockingStorage{}, nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrSessionUnauthorized)
}
