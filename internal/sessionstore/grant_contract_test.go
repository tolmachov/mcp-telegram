package sessionstore

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fsouza/fake-gcs-server/fakestorage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testGrantFamily = "fedcba9876543210fedcba9876543210"

func grantStores(t *testing.T) map[string]Store {
	t.Helper()
	fs, err := NewFS(filepath.Join(t.TempDir(), "store"))
	require.NoError(t, err)
	return map[string]Store{
		"memory": NewMemory(),
		"fs":     fs,
		"gcs":    newGCSContractStore(t),
	}
}

func newGCSContractStore(t *testing.T) *GCS {
	t.Helper()
	emulator, err := fakestorage.NewServerWithOptions(fakestorage.Options{NoListener: true})
	require.NoError(t, err)
	t.Cleanup(emulator.Stop)
	const bucket = "session-contract"
	emulator.CreateBucket(bucket) //nolint:staticcheck // Test helper has no result; a duplicate bucket panics internally.
	return &GCS{bucket: emulator.Client().Bucket(bucket)}
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

			results := make(chan GrantRotation, 2)
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

			seen := map[GrantRotation]int{}
			for range 2 {
				seen[<-results]++
				require.NoError(t, <-errs)
			}
			assert.Equal(t, 1, seen[GrantRotated])
			assert.Equal(t, 1, seen[GrantReplay])

			result, err := store.RotateGrant(ctx, testGrantFamily, 1)
			require.NoError(t, err)
			assert.Equal(t, GrantReplay, result, "a replay must revoke the entire family")
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
			assert.Equal(t, GrantMissing, result)
		})
	}
}

func TestGrantStoreRejectsMalformedIdentity(t *testing.T) {
	for name, store := range grantStores(t) {
		t.Run(name, func(t *testing.T) {
			_, err := store.RedeemCode(t.Context(), "../not-a-family", testSID, time.Now().Add(time.Hour))
			require.ErrorIs(t, err, errInvalidStoreSID)
			_, err = store.RotateGrant(t.Context(), "../not-a-family", 0)
			require.ErrorIs(t, err, errInvalidStoreSID)
			require.ErrorIs(t, store.RevokeGrant(t.Context(), "../not-a-family"), errInvalidStoreSID)
		})
	}
}

func TestSessionStoreRejectsMalformedIdentity(t *testing.T) {
	for name, store := range grantStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			bad := "../../outside"
			require.ErrorIs(t, store.Session(1, bad, nil).StoreSession(ctx, []byte("x")), errInvalidStoreSID)
			_, err := store.Exists(ctx, 1, bad)
			require.ErrorIs(t, err, errInvalidStoreSID)
			require.ErrorIs(t, store.Delete(ctx, 1, bad), errInvalidStoreSID)
			require.ErrorIs(t, store.Revoke(ctx, 1, bad), errInvalidStoreSID)
			_, err = store.Revoked(ctx, 1, bad)
			require.ErrorIs(t, err, errInvalidStoreSID)
			require.ErrorIs(t, store.DeleteRevoked(ctx, 1, bad), errInvalidStoreSID)
		})
	}
}

func TestGCSGrantCorruptionFailsWithoutOverwrite(t *testing.T) {
	store := newGCSContractStore(t)
	ctx := t.Context()
	object := store.bucket.Object(grantObjectName(testGrantFamily))
	w := object.NewWriter(ctx)
	_, err := io.WriteString(w, "not-json")
	require.NoError(t, err)
	require.NoError(t, w.Close())

	_, err = store.RotateGrant(ctx, testGrantFamily, 0)
	require.ErrorContains(t, err, "parsing grant")
	r, err := object.NewReader(ctx)
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	assert.Equal(t, "not-json", string(data), "a failed read must not rewrite authorization state")
}
