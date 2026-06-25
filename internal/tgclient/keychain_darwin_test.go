//go:build darwin

package tgclient

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/secret"
)

// fakeVault is a minimal sessionVault for the adapter. It lets us assert the
// translation of secret.ErrNotFound → session.ErrNotFound without spinning up
// a real Keychain, which would require a signed binary and a user prompt.
type fakeVault struct {
	session    []byte
	loadErr    error
	storeErr   error
	deleteErr  error
	loadCalls  int
	storeCalls int
	delCalls   int
	lastStored []byte
}

func (f *fakeVault) SessionLoad() ([]byte, error) {
	f.loadCalls++
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return f.session, nil
}

func (f *fakeVault) SessionStore(data []byte) error {
	f.storeCalls++
	if f.storeErr != nil {
		return f.storeErr
	}
	f.lastStored = append([]byte(nil), data...)
	f.session = f.lastStored
	return nil
}

func (f *fakeVault) SessionDelete() error {
	f.delCalls++
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.session = nil
	return nil
}

func newTestSessionStorage(v sessionVault) *SessionStorage {
	return &SessionStorage{vault: v}
}

func TestLoadSessionTranslatesNotFound(t *testing.T) {
	fv := &fakeVault{loadErr: secret.ErrNotFound}
	s := newTestSessionStorage(fv)

	_, err := s.LoadSession(context.Background())
	require.Error(t, err)
	// The gotd/td session package uses its own sentinel to decide "no session,
	// force login" — if we leaked secret.ErrNotFound up, the client would
	// panic or treat it as a real error instead.
	assert.True(t, errors.Is(err, session.ErrNotFound))
}

func TestLoadSessionPassesThroughOtherErrors(t *testing.T) {
	boom := errors.New("keychain unavailable")
	fv := &fakeVault{loadErr: boom}
	s := newTestSessionStorage(fv)

	_, err := s.LoadSession(context.Background())
	require.ErrorIs(t, err, boom)
}

func TestLoadSessionReturnsData(t *testing.T) {
	fv := &fakeVault{session: []byte("bytes")}
	s := newTestSessionStorage(fv)

	got, err := s.LoadSession(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []byte("bytes"), got)
}

func TestStoreSessionDelegates(t *testing.T) {
	fv := &fakeVault{}
	s := newTestSessionStorage(fv)

	require.NoError(t, s.StoreSession(context.Background(), []byte("payload")))
	assert.Equal(t, 1, fv.storeCalls)
	assert.Equal(t, []byte("payload"), fv.lastStored)
}

func TestDeleteSessionDelegates(t *testing.T) {
	fv := &fakeVault{session: []byte("x")}
	s := newTestSessionStorage(fv)

	require.NoError(t, s.DeleteSession())
	assert.Equal(t, 1, fv.delCalls)
	assert.Empty(t, fv.session)
}
