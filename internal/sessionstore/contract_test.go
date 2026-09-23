package sessionstore_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore/sessionstoretest"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// The contract tests live in an external package so the in-memory test store,
// which imports sessionstore, runs against the same checks as the real
// backends.

const (
	testSID         = "0123456789abcdef0123456789abcdef"
	testGrantFamily = "fedcba9876543210fedcba9876543210"
)

func newFS(t *testing.T) *sessionstore.FS {
	t.Helper()
	fs, err := sessionstore.NewFS(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	return fs
}

func grantStores(t *testing.T) map[string]sessionstore.Store {
	t.Helper()
	return map[string]sessionstore.Store{
		"memory": sessionstoretest.NewMemory(),
		"fs":     newFS(t),
		"gcs":    sessionstore.NewTestGCS(t),
	}
}

func TestGrantStoreContract(t *testing.T) {
	for name, store := range grantStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			expiresAt := time.Now().Add(time.Hour)

			created, err := store.RedeemCode(ctx, testGrantFamily, testSID, expiresAt)
			require.NoError(t, err)
			require.True(t, created)

			created, err = store.RedeemCode(ctx, testGrantFamily, testSID, expiresAt)
			require.NoError(t, err)
			assert.False(t, created, "authorization code redemption must be single-use")

			results := make(chan sessionstore.GrantRotation, 2)
			errs := make(chan error, 2)
			var start sync.WaitGroup
			start.Add(1)
			for range 2 {
				go func() {
					start.Wait()
					result, rotateErr := store.RotateGrant(context.Background(), testGrantFamily, 0)
					results <- result
					errs <- rotateErr
				}()
			}
			start.Done()

			seen := map[sessionstore.GrantRotation]int{}
			for range 2 {
				seen[<-results]++
				require.NoError(t, <-errs)
			}
			assert.Equal(t, 1, seen[sessionstore.GrantRotated])
			assert.Equal(t, 1, seen[sessionstore.GrantReplay])

			result, err := store.RotateGrant(ctx, testGrantFamily, 1)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.GrantReplay, result, "a replay must revoke the entire family")
		})
	}
}

func TestGrantStoreSweepContract(t *testing.T) {
	for name, store := range grantStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			created, err := store.RedeemCode(ctx, testGrantFamily, testSID, time.Now().Add(-time.Minute))
			require.NoError(t, err)
			require.True(t, created)
			require.NoError(t, store.SweepAuthState(ctx, time.Now()))

			result, err := store.RotateGrant(ctx, testGrantFamily, 0)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.GrantMissing, result)
		})
	}
}

func TestGrantStoreRejectsMalformedIdentity(t *testing.T) {
	for name, store := range grantStores(t) {
		t.Run(name, func(t *testing.T) {
			_, err := store.RedeemCode(t.Context(), "../not-a-family", testSID, time.Now().Add(time.Hour))
			require.ErrorIs(t, err, sessionstore.ErrInvalidSID)
			_, err = store.RotateGrant(t.Context(), "../not-a-family", 0)
			require.ErrorIs(t, err, sessionstore.ErrInvalidSID)
			require.ErrorIs(t, store.RevokeGrant(t.Context(), "../not-a-family"), sessionstore.ErrInvalidSID)
		})
	}
}

func TestSessionStoreRejectsMalformedIdentity(t *testing.T) {
	for name, store := range grantStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			bad := "../../outside"
			require.ErrorIs(t, store.Session(1, bad, nil).StoreSession(ctx, []byte("x")), sessionstore.ErrInvalidSID)
			_, err := store.Exists(ctx, 1, bad)
			require.ErrorIs(t, err, sessionstore.ErrInvalidSID)
			require.ErrorIs(t, store.Delete(ctx, 1, bad), sessionstore.ErrInvalidSID)
			require.ErrorIs(t, store.Revoke(ctx, 1, bad), sessionstore.ErrInvalidSID)
			_, err = store.Revoked(ctx, 1, bad)
			require.ErrorIs(t, err, sessionstore.ErrInvalidSID)
			require.ErrorIs(t, store.DeleteRevoked(ctx, 1, bad), sessionstore.ErrInvalidSID)
		})
	}
}

// TestRevokeTombstone exercises the tombstone lifecycle on every real backend:
// Revoke marks + deletes the blob, Revoked reflects it, tombstones are listed
// by ListRevoked but NOT by List (not mistaken for sessions), and DeleteRevoked
// clears them.
func TestRevokeTombstone(t *testing.T) {
	const user = tgid.UserID(55)
	const sid = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name string
		make func(t *testing.T) sessionstore.Store
	}{
		{"memory", func(_ *testing.T) sessionstore.Store { return sessionstoretest.NewMemory() }},
		{"fs", func(t *testing.T) sessionstore.Store { return newFS(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store := tc.make(t)
			if err := store.Session(user, sid, nil).StoreSession(ctx, []byte("blob")); err != nil {
				t.Fatalf("StoreSession: %v", err)
			}

			if r, err := store.Revoked(ctx, user, sid); err != nil || r {
				t.Fatalf("Revoked before revoke = (%v, %v), want (false, nil)", r, err)
			}
			if err := store.Revoke(ctx, user, sid); err != nil {
				t.Fatalf("Revoke: %v", err)
			}
			if r, err := store.Revoked(ctx, user, sid); err != nil || !r {
				t.Errorf("Revoked after revoke = (%v, %v), want (true, nil)", r, err)
			}
			// The blob is gone; the tombstone is not listed as a session.
			if ok, _ := store.Exists(ctx, user, sid); ok {
				t.Error("Revoke must delete the session blob")
			}
			sessions, err := store.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(sessions) != 0 {
				t.Errorf("List returned %d sessions, want 0 (tombstone must not appear as a session)", len(sessions))
			}
			revoked, err := store.ListRevoked(ctx)
			if err != nil {
				t.Fatalf("ListRevoked: %v", err)
			}
			if len(revoked) != 1 || revoked[0].UserID != user || revoked[0].SID != sid {
				t.Errorf("ListRevoked = %+v, want one tombstone for (%d,%s)", revoked, user, sid)
			}

			if err := store.DeleteRevoked(ctx, user, sid); err != nil {
				t.Fatalf("DeleteRevoked: %v", err)
			}
			if r, _ := store.Revoked(ctx, user, sid); r {
				t.Error("Revoked after DeleteRevoked = true, want false")
			}
		})
	}
}
