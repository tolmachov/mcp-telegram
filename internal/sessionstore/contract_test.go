package sessionstore_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/keyring"
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

// stores returns an empty Encrypted store over every backend.
func stores(t *testing.T) map[string]sessionstore.Store {
	t.Helper()
	key := make([]byte, keyring.MasterKeyLen)
	_, err := rand.Read(key)
	require.NoError(t, err)
	ring, err := keyring.Parse([]string{base64.StdEncoding.EncodeToString(key)})
	require.NoError(t, err)
	cipher := sessionstore.NewCipher(ring, "https://mcp.example.com")
	fs, err := sessionstore.NewFS(filepath.Join(t.TempDir(), "sessions"))
	require.NoError(t, err)
	return map[string]sessionstore.Store{
		"memory": sessionstoretest.New(t),
		"fs":     sessionstore.Encrypted(fs, cipher),
		"gcs":    sessionstore.Encrypted(sessionstore.NewTestGCS(t), cipher),
	}
}

func TestGrantStoreContract(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now()
			expiresAt := now.Add(time.Hour)

			created, err := store.RedeemCode(ctx, testGrantFamily, expiresAt)
			require.NoError(t, err)
			require.True(t, created)

			created, err = store.RedeemCode(ctx, testGrantFamily, expiresAt)
			require.NoError(t, err)
			assert.False(t, created, "authorization code redemption must be single-use")

			results := make(chan sessionstore.GrantRotation, 2)
			errs := make(chan error, 2)
			var start sync.WaitGroup
			start.Add(1)
			for range 2 {
				go func() {
					start.Wait()
					result, rotateErr := store.RotateGrant(context.Background(), testGrantFamily, 0, now)
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

			result, err := store.RotateGrant(ctx, testGrantFamily, 1, now)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.GrantReplay, result, "a replay must revoke the entire family")
		})
	}
}

func TestGrantStoreRotateAndRevokeContract(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now()
			created, err := store.RedeemCode(ctx, testGrantFamily, now.Add(time.Hour))
			require.NoError(t, err)
			require.True(t, created)

			for gen := range int64(3) {
				result, err := store.RotateGrant(ctx, testGrantFamily, gen, now)
				require.NoError(t, err)
				require.Equal(t, sessionstore.GrantRotated, result, "generation %d", gen)
			}
			require.NoError(t, store.RevokeGrant(ctx, testGrantFamily))
			result, err := store.RotateGrant(ctx, testGrantFamily, 3, now)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.GrantReplay, result, "a revoked family must not rotate")

			require.NoError(t, store.RevokeGrant(ctx, "00000000000000000000000000000000"), "revoking a missing family is a no-op")
			result, err = store.RotateGrant(ctx, "00000000000000000000000000000000", 0, now)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.GrantMissing, result)
		})
	}
}

func TestGrantStoreExpiryContract(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			expiresAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			created, err := store.RedeemCode(ctx, testGrantFamily, expiresAt)
			require.NoError(t, err)
			require.True(t, created)

			// Expiry follows the caller's clock, not the backend's.
			result, err := store.RotateGrant(ctx, testGrantFamily, 0, expiresAt.Add(-time.Second))
			require.NoError(t, err)
			assert.Equal(t, sessionstore.GrantRotated, result)
			result, err = store.RotateGrant(ctx, testGrantFamily, 1, expiresAt)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.GrantMissing, result, "a grant is expired from ExpiresAt on")

			// A record without an expiry is malformed and counts as expired.
			const zeroFamily = "00000000000000000000000000000001"
			created, err = store.RedeemCode(ctx, zeroFamily, time.Time{})
			require.NoError(t, err)
			require.True(t, created)
			result, err = store.RotateGrant(ctx, zeroFamily, 0, expiresAt)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.GrantMissing, result)
		})
	}
}

func TestGrantStoreSweepContract(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now()
			const liveFamily = "00000000000000000000000000000002"
			created, err := store.RedeemCode(ctx, testGrantFamily, now.Add(-time.Minute))
			require.NoError(t, err)
			require.True(t, created)
			created, err = store.RedeemCode(ctx, liveFamily, now.Add(time.Hour))
			require.NoError(t, err)
			require.True(t, created)
			require.NoError(t, store.SweepAuthState(ctx, now))

			// Redeeming again creates a record only where none is left.
			created, err = store.RedeemCode(ctx, testGrantFamily, now.Add(time.Hour))
			require.NoError(t, err)
			assert.True(t, created, "the expired grant must be swept")
			created, err = store.RedeemCode(ctx, liveFamily, now.Add(time.Hour))
			require.NoError(t, err)
			assert.False(t, created, "a live grant must survive the sweep")
		})
	}
}

func TestGrantStoreRejectsMalformedIdentity(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			_, err := store.RedeemCode(ctx, "../not-a-family", time.Now().Add(time.Hour))
			require.ErrorIs(t, err, sessionstore.ErrInvalidSID)
			_, err = store.RotateGrant(ctx, "../not-a-family", 0, time.Now())
			require.ErrorIs(t, err, sessionstore.ErrInvalidSID)
			require.ErrorIs(t, store.RevokeGrant(ctx, "../not-a-family"), sessionstore.ErrInvalidSID)
		})
	}
}

func TestSessionStoreRejectsMalformedIdentity(t *testing.T) {
	for name, store := range stores(t) {
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

// TestRevokeTombstone exercises the tombstone lifecycle on every backend:
// Revoke marks + deletes the blob, Revoked reflects it, tombstones are listed
// by ListRevoked but NOT by List (not mistaken for sessions), and DeleteRevoked
// clears them.
func TestRevokeTombstone(t *testing.T) {
	const user = tgid.UserID(55)
	userKey := make([]byte, 32)
	_, err := rand.Read(userKey)
	require.NoError(t, err)
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			require.NoError(t, store.Session(user, testSID, userKey).StoreSession(ctx, []byte("blob")))

			revoked, err := store.Revoked(ctx, user, testSID)
			require.NoError(t, err)
			require.False(t, revoked)
			require.NoError(t, store.Revoke(ctx, user, testSID))
			revoked, err = store.Revoked(ctx, user, testSID)
			require.NoError(t, err)
			assert.True(t, revoked)

			// The blob is gone; the tombstone is not listed as a session.
			exists, err := store.Exists(ctx, user, testSID)
			require.NoError(t, err)
			assert.False(t, exists, "Revoke must delete the session blob")
			sessions, err := store.List(ctx)
			require.NoError(t, err)
			assert.Empty(t, sessions, "a tombstone must not appear as a session")
			tombstones, err := store.ListRevoked(ctx)
			require.NoError(t, err)
			require.Len(t, tombstones, 1)
			assert.Equal(t, user, tombstones[0].UserID)
			assert.Equal(t, testSID, tombstones[0].SID)

			require.NoError(t, store.DeleteRevoked(ctx, user, testSID))
			revoked, err = store.Revoked(ctx, user, testSID)
			require.NoError(t, err)
			assert.False(t, revoked)
		})
	}
}
