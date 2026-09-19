package tgclient

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	errPassword := errors.New("PASSWORD_HASH_INVALID")
	f.recordPasswordError(errPassword)
	assert.Equal(t, QRPasswordNeeded, f.State())
	assert.ErrorIs(t, f.Err(), errPassword)

	errFailed := errors.New("expired")
	f.fail(errFailed)
	assert.Equal(t, QRFailed, f.State())
	assert.ErrorIs(t, f.Err(), errFailed)
}
