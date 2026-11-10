package sessionstore_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

func TestCheckSessionContract(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			const user = tgid.UserID(55)
			blob := store.Session(user, testSID, make([]byte, 32))

			state, err := sessionstore.CheckSession(ctx, store, user, testSID)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.SessionMissing, state)

			require.NoError(t, blob.StoreSession(ctx, []byte("session")))
			state, err = sessionstore.CheckSession(ctx, store, user, testSID)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.SessionPresent, state)

			require.NoError(t, store.Revoke(ctx, user, testSID))
			state, err = sessionstore.CheckSession(ctx, store, user, testSID)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.SessionRevoked, state)

			// A running Telegram client may write the blob again after revocation.
			require.NoError(t, blob.StoreSession(ctx, []byte("restored")))
			state, err = sessionstore.CheckSession(ctx, store, user, testSID)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.SessionRevoked, state)

			_, err = sessionstore.CheckSession(ctx, store, user, "../bad")
			require.ErrorIs(t, err, sessionstore.ErrInvalidSID)
		})
	}
}

// observedSessionStore substitutes only the two observations; CheckSession
// still owns their execution and interpretation.
type observedSessionStore struct {
	sessionstore.Store
	exists  func(context.Context) (bool, error)
	revoked func(context.Context) (bool, error)
}

func (s observedSessionStore) Exists(ctx context.Context, _ tgid.UserID, _ string) (bool, error) {
	return s.exists(ctx)
}

func (s observedSessionStore) Revoked(ctx context.Context, _ tgid.UserID, _ string) (bool, error) {
	return s.revoked(ctx)
}

func TestCheckSessionObservationPrecedence(t *testing.T) {
	existsErr := errors.New("session read failed")
	revokedErr := errors.New("revocation read failed")
	for _, tc := range []struct {
		name                  string
		exists, revoked       bool
		existsErr, revokedErr error
		want                  sessionstore.SessionState
		wantErr               error
	}{
		{name: "missing", want: sessionstore.SessionMissing},
		{name: "present", exists: true, want: sessionstore.SessionPresent},
		{name: "revoked and missing", revoked: true, want: sessionstore.SessionRevoked},
		{name: "revoked and present", exists: true, revoked: true, want: sessionstore.SessionRevoked},
		{name: "revoked despite existence failure", revoked: true, existsErr: existsErr, want: sessionstore.SessionRevoked},
		{name: "existence failure", existsErr: existsErr, wantErr: existsErr},
		{name: "revocation failure despite presence", exists: true, revokedErr: revokedErr, wantErr: revokedErr},
		{name: "revocation failure despite missing", revokedErr: revokedErr, wantErr: revokedErr},
		{name: "both fail", existsErr: existsErr, revokedErr: revokedErr, wantErr: revokedErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := observedSessionStore{
				exists:  func(context.Context) (bool, error) { return tc.exists, tc.existsErr },
				revoked: func(context.Context) (bool, error) { return tc.revoked, tc.revokedErr },
			}
			state, err := sessionstore.CheckSession(t.Context(), store, 55, testSID)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Equal(t, sessionstore.SessionMissing, state, "a failed check must not permit refresh")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, state)
		})
	}
}

func TestCheckSessionObservationsOverlap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		existsStarted, revokedStarted := make(chan struct{}), make(chan struct{})
		store := observedSessionStore{
			exists: func(ctx context.Context) (bool, error) {
				close(existsStarted)
				select {
				case <-revokedStarted:
					return true, nil
				case <-ctx.Done():
					return false, ctx.Err()
				}
			},
			revoked: func(ctx context.Context) (bool, error) {
				close(revokedStarted)
				select {
				case <-existsStarted:
					return false, nil
				case <-ctx.Done():
					return false, ctx.Err()
				}
			},
		}
		state, err := sessionstore.CheckSession(ctx, store, 55, testSID)
		require.NoError(t, err, "both observations must start before either finishes")
		assert.Equal(t, sessionstore.SessionPresent, state)
	})
}

func TestCheckSessionCancellationJoinsObservations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		existsDone := make(chan struct{})
		store := observedSessionStore{
			exists: func(ctx context.Context) (bool, error) {
				defer close(existsDone)
				<-ctx.Done()
				return false, ctx.Err()
			},
			revoked: func(ctx context.Context) (bool, error) {
				<-ctx.Done()
				return false, ctx.Err()
			},
		}
		state, err := sessionstore.CheckSession(ctx, store, 55, testSID)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, sessionstore.SessionMissing, state)
		select {
		case <-existsDone:
		default:
			t.Fatal("session check returned before the existence observation stopped")
		}
	})
}
