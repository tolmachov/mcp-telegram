package tgclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestQRFlowRejectsConcurrentPasswordSubmission(t *testing.T) {
	f := &QRFlow{
		state:      QRPasswordNeeded,
		passwordCh: make(chan string, 1),
	}
	require.True(t, f.SubmitPassword("first"))
	assert.Equal(t, QRPasswordVerifying, f.State())
	assert.False(t, f.SubmitPassword("second"), "a concurrent submit must not consume another attempt")
	assert.Equal(t, "first", <-f.passwordCh)
}

func TestQRFlowSessionDataIsDefensiveCopy(t *testing.T) {
	f := &QRFlow{}
	f.complete(QRUser{ID: 7, Username: "user"}, []byte("session"))

	first, ok := f.SessionData()
	require.True(t, ok)
	first[0] = 'X'
	second, ok := f.SessionData()
	require.True(t, ok)
	assert.Equal(t, []byte("session"), second)

	user, ok := f.User()
	require.True(t, ok)
	assert.Equal(t, QRUser{ID: 7, Username: "user"}, user)
}

func TestQRFlowStateTransitions(t *testing.T) {
	f := &QRFlow{state: QRWaiting}
	f.setToken("tg://login?token=one")
	url, ok := f.TokenURL()
	require.True(t, ok)
	assert.Equal(t, "tg://login?token=one", url)

	errFailed := errors.New("expired")
	f.fail(errFailed)
	assert.Equal(t, QRFailed, f.State())
	assert.ErrorIs(t, f.Err(), errFailed)
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
	require.Eventually(t, func() bool { return f.State() == QRPasswordNeeded }, time.Second, time.Millisecond)
}

func TestQRFlowPasswordLoopSpendsAttemptBudget(t *testing.T) {
	wrong := tgerr.New(400, "PASSWORD_HASH_INVALID")
	f, res := runPasswordLoop(t, func(context.Context, string) (*tg.AuthAuthorization, error) {
		return nil, wrong
	})

	for attempt := range qrPasswordAttempts {
		awaitPrompt(t, f)
		rejected, left := f.Rejections()
		assert.Equal(t, attempt, rejected)
		assert.Equal(t, qrPasswordAttempts-attempt, left)
		require.True(t, f.SubmitPassword("wrong"))
	}

	require.ErrorContains(t, (<-res).err, "too many wrong 2FA password attempts")
	rejected, left := f.Rejections()
	assert.Equal(t, qrPasswordAttempts, rejected)
	assert.Zero(t, left)
	assert.Equal(t, QRPasswordVerifying, f.State(), "a spent budget must not reopen the prompt")
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
	assert.Equal(t, QRPasswordVerifying, f.State())
	rejected, _ := f.Rejections()
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
	rejected, left := f.Rejections()
	assert.Equal(t, 1, rejected)
	assert.Equal(t, qrPasswordAttempts-1, left)
	require.True(t, f.SubmitPassword("right"))
	got := <-res
	require.NoError(t, got.err)
	assert.Same(t, want, got.auth)
}
