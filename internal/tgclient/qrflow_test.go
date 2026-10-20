package tgclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gotd/td/session"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestQRFlowRejectsConcurrentPasswordSubmission(t *testing.T) {
	f := &QRFlow{
		state:      QRPasswordNeeded,
		passwordCh: make(chan string, 1),
	}
	require.True(t, f.SubmitPassword("first"))
	assert.Equal(t, QRPasswordVerifying, f.Observe().State)
	assert.False(t, f.SubmitPassword("second"), "a concurrent submit must not consume another attempt")
	assert.Equal(t, "first", <-f.passwordCh)
}

func TestQRFlowStateTransitions(t *testing.T) {
	f := &QRFlow{state: QRWaiting}
	f.setToken("tg://login?token=one")
	url := f.Observe().TokenURL
	require.NotEmpty(t, url)
	assert.Equal(t, "tg://login?token=one", url)

	errFailed := errors.New("expired")
	f.fail(errFailed)
	assert.Equal(t, QRFailed, f.Observe().State)
	assert.ErrorIs(t, f.Observe().Err, errFailed)
}

// loopResult is what passwordLoop returned.
type loopResult struct {
	auth *tg.AuthAuthorization
	err  error
}

// runPasswordLoop runs passwordLoop on a fresh flow with check standing in
// for Telegram's password RPC.
func runPasswordLoop(t *testing.T, check func(context.Context, string) (*tg.AuthAuthorization, error)) (*QRFlow, <-chan loopResult) {
	t.Helper()
	f := &QRFlow{state: QRWaiting, passwordCh: make(chan string, 1)}
	res := make(chan loopResult, 1)
	go func() {
		auth, err := f.passwordLoop(t.Context(), check)
		res <- loopResult{auth, err}
	}()
	return f, res
}

// awaitPrompt waits for passwordLoop to open QRPasswordNeeded.
func awaitPrompt(t *testing.T, f *QRFlow) {
	t.Helper()
	require.Eventually(t, func() bool { return f.Observe().State == QRPasswordNeeded }, time.Second, time.Millisecond)
}

func TestQRFlowPasswordLoopSpendsAttemptBudget(t *testing.T) {
	wrong := tgerr.New(400, "PASSWORD_HASH_INVALID")
	f, res := runPasswordLoop(t, func(context.Context, string) (*tg.AuthAuthorization, error) {
		return nil, wrong
	})

	for attempt := range qrPasswordAttempts {
		awaitPrompt(t, f)
		observed := f.Observe()
		rejected, left := observed.Rejected, observed.AttemptsLeft
		assert.Equal(t, attempt, rejected)
		assert.Equal(t, qrPasswordAttempts-attempt, left)
		require.True(t, f.SubmitPassword("wrong"))
	}

	require.ErrorContains(t, (<-res).err, "too many wrong 2FA password attempts")
	observed := f.Observe()
	rejected, left := observed.Rejected, observed.AttemptsLeft
	assert.Equal(t, qrPasswordAttempts, rejected)
	assert.Zero(t, left)
	assert.Equal(t, QRPasswordVerifying, f.Observe().State, "a spent budget must not reopen the prompt")
	assert.False(t, f.SubmitPassword("late"), "no password is accepted once the budget is spent")
}

func TestQRFlowPasswordLoopStopsOnOtherErrors(t *testing.T) {
	f, res := runPasswordLoop(t, func(context.Context, string) (*tg.AuthAuthorization, error) {
		return nil, tgerr.New(400, "SRP_ID_INVALID")
	})

	awaitPrompt(t, f)
	require.True(t, f.SubmitPassword("pw"))

	err := (<-res).err
	assert.True(t, tgerr.Is(err, "SRP_ID_INVALID"), "got %v", err)
	assert.Equal(t, QRPasswordVerifying, f.Observe().State)
	rejected := f.Observe().Rejected
	assert.Zero(t, rejected, "only a wrong password counts as a rejection")
}

func TestQRFlowPasswordLoopReturnsAuthorization(t *testing.T) {
	want := &tg.AuthAuthorization{}
	f, res := runPasswordLoop(t, func(_ context.Context, pw string) (*tg.AuthAuthorization, error) {
		if pw != "right" {
			return nil, tgerr.New(400, "PASSWORD_HASH_INVALID")
		}
		return want, nil
	})

	awaitPrompt(t, f)
	require.True(t, f.SubmitPassword("wrong"))
	awaitPrompt(t, f)
	observed := f.Observe()
	rejected, left := observed.Rejected, observed.AttemptsLeft
	assert.Equal(t, 1, rejected)
	assert.Equal(t, qrPasswordAttempts-1, left)
	require.True(t, f.SubmitPassword("right"))
	got := <-res
	require.NoError(t, got.err)
	assert.Same(t, want, got.auth)
}

// Removing token revision accounting or publishing mismatched counters must
// change what a caller observes, independently of the HTTP presentation.
func TestQRFlowObservationTracksTokenAndPasswordState(t *testing.T) {
	f := &QRFlow{state: QRWaiting, passwordCh: make(chan string, 1)}
	initial := f.Observe()
	assert.Empty(t, initial.TokenURL)
	assert.Zero(t, initial.QRRevision)
	assert.Equal(t, qrPasswordAttempts, initial.AttemptsLeft)
	_, ok := f.Result()
	assert.False(t, ok)

	f.setToken("tg://login?token=one")
	f.setToken("tg://login?token=one")
	first := f.Observe()
	assert.Equal(t, "tg://login?token=one", first.TokenURL)
	assert.Equal(t, 1, first.QRRevision)
	f.setToken("tg://login?token=two")
	rotated := f.Observe()
	assert.Equal(t, "tg://login?token=two", rotated.TokenURL)
	assert.Equal(t, 2, rotated.QRRevision)
	assert.Equal(t, "tg://login?token=one", first.TokenURL)

	f.setState(QRPasswordNeeded)
	require.True(t, f.SubmitPassword("password"))
	checking := f.Observe()
	assert.Equal(t, QRPasswordVerifying, checking.State)
	assert.Zero(t, checking.Rejected)
	assert.Equal(t, qrPasswordAttempts, checking.AttemptsLeft)
	f.rejectPassword()
	f.setState(QRPasswordNeeded)
	retry := f.Observe()
	assert.Equal(t, QRPasswordNeeded, retry.State)
	assert.Equal(t, 1, retry.Rejected)
	assert.Equal(t, qrPasswordAttempts-1, retry.AttemptsLeft)
}

func TestQRFlowResultIsReadyOnlyOnSuccess(t *testing.T) {
	f := &QRFlow{state: QRPasswordVerifying}
	_, ok := f.Result()
	require.False(t, ok)
	f.complete(QRUser{ID: 7, Username: "user"}, []byte("session"))
	require.Equal(t, QRDone, f.Observe().State)
	result, ok := f.Result()
	require.True(t, ok)
	assert.Equal(t, QRUser{ID: 7, Username: "user"}, result.User)
	result.SessionData[0] = 'X'
	again, ok := f.Result()
	require.True(t, ok)
	assert.Equal(t, []byte("session"), again.SessionData)

	failed := &QRFlow{}
	failed.fail(errors.New("expired"))
	assert.Equal(t, QRFailed, failed.Observe().State)
	assert.ErrorContains(t, failed.Observe().Err, "expired")
	_, ok = failed.Result()
	assert.False(t, ok)
}

func TestQRFlowWaitsForFinalBootstrapSession(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := &QRFlow{state: QRWaiting, cancel: cancel}
	storage := &session.StorageMemory{}
	require.NoError(t, storage.StoreSession(ctx, []byte("before teardown")))
	authenticated := make(chan struct{})
	teardown := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(teardown) }) })
	runClient := func(ctx context.Context, authenticate func(context.Context) error) error {
		if err := authenticate(ctx); err != nil {
			return err
		}
		close(authenticated)
		select {
		case <-teardown:
		case <-ctx.Done():
			return ctx.Err()
		}
		// gotd persists its final session while disconnecting, after authentication.
		if err := storage.StoreSession(ctx, []byte("final session")); err != nil {
			return fmt.Errorf("storing final test session: %w", err)
		}
		return nil
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		f.run(ctx, runClient, func(context.Context) (QRUser, error) {
			return QRUser{ID: 7, Username: "user"}, nil
		}, storage)
	}()
	select {
	case <-authenticated:
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap did not authenticate")
	}
	assert.NotEqual(t, QRDone, f.Observe().State, "authentication alone must not publish success")
	_, ok := f.Result()
	assert.False(t, ok, "session is not final while the bootstrap connection is live")
	release.Do(func() { close(teardown) })
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("QR flow did not finish after bootstrap teardown")
	}
	require.Equal(t, QRDone, f.Observe().State)
	result, ok := f.Result()
	require.True(t, ok)
	assert.Equal(t, []byte("final session"), result.SessionData)
	assert.Equal(t, QRUser{ID: 7, Username: "user"}, result.User)
	f.Abort()
	assert.Equal(t, QRDone, f.Observe().State, "aborting after success cannot invalidate the result")
}
