package sessionstore_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
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

// TestGrantStoreCASContract pins the compare-and-swap every backend's
// StoreGrant must keep, step by step and without concurrency.
func TestGrantStoreCASContract(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			first := sessionstore.GrantRecord{ExpiresAt: time.Now().Add(time.Hour).UTC()}
			require.NoError(t, store.StoreGrant(ctx, testGrantFamily, first, 0))
			require.ErrorIs(t, store.StoreGrant(ctx, testGrantFamily, first, 0), sessionstore.ErrGrantConflict,
				"version 0 creates only when no record exists")

			got, version, err := store.LoadGrant(ctx, testGrantFamily)
			require.NoError(t, err)
			require.NotZero(t, version)
			assert.Equal(t, first, got)

			second := first
			second.Generation = 1
			require.NoError(t, store.StoreGrant(ctx, testGrantFamily, second, version))
			third := second
			third.Generation = 2
			require.ErrorIs(t, store.StoreGrant(ctx, testGrantFamily, third, version), sessionstore.ErrGrantConflict,
				"a version that was already written over is stale")

			got, _, err = store.LoadGrant(ctx, testGrantFamily)
			require.NoError(t, err)
			assert.Equal(t, second, got)
		})
	}
}

func TestGrantStoreContract(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now()
			expiresAt := now.Add(time.Hour)

			created, err := sessionstore.RedeemCode(ctx, store, testGrantFamily, expiresAt)
			require.NoError(t, err)
			require.True(t, created)

			created, err = sessionstore.RedeemCode(ctx, store, testGrantFamily, expiresAt)
			require.NoError(t, err)
			assert.False(t, created, "authorization code redemption must be single-use")

			results := make(chan sessionstore.GrantRotation, 2)
			errs := make(chan error, 2)
			var start sync.WaitGroup
			start.Add(1)
			for range 2 {
				go func() {
					start.Wait()
					result, rotateErr := sessionstore.RotateGrant(context.Background(), store, testGrantFamily, 0, now)
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

			result, err := sessionstore.RotateGrant(ctx, store, testGrantFamily, 1, now)
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
			created, err := sessionstore.RedeemCode(ctx, store, testGrantFamily, now.Add(time.Hour))
			require.NoError(t, err)
			require.True(t, created)

			for gen := range int64(3) {
				result, err := sessionstore.RotateGrant(ctx, store, testGrantFamily, gen, now)
				require.NoError(t, err)
				require.Equal(t, sessionstore.GrantRotated, result, "generation %d", gen)
			}
			grant, version, err := store.LoadGrant(ctx, testGrantFamily)
			require.NoError(t, err)
			assert.NotZero(t, version)
			assert.NotEmpty(t, grant.WriteID, "every grant write stamps its record")
			assert.Equal(t, sessionstore.GrantRecord{Generation: 3, ExpiresAt: grant.ExpiresAt, WriteID: grant.WriteID}, grant)

			require.NoError(t, sessionstore.RevokeGrant(ctx, store, testGrantFamily))
			result, err := sessionstore.RotateGrant(ctx, store, testGrantFamily, 3, now)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.GrantReplay, result, "a revoked family must not rotate")

			require.NoError(t, sessionstore.RevokeGrant(ctx, store, "00000000000000000000000000000000"), "revoking a missing family is a no-op")
			result, err = sessionstore.RotateGrant(ctx, store, "00000000000000000000000000000000", 0, now)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.GrantMissing, result)
		})
	}
}

// lostResponse lands the first grant write it is given, then reports that
// write as failed with err, the way a write whose response never arrived does.
type lostResponse struct {
	sessionstore.Store
	err  error
	lost bool
}

func (s *lostResponse) StoreGrant(ctx context.Context, family string, grant sessionstore.GrantRecord, version int64) error {
	if err := s.Store.StoreGrant(ctx, family, grant, version); err != nil {
		return err //nolint:wrapcheck // passes the backend's outcome through unchanged
	}
	if s.lost {
		return nil
	}
	s.lost = true
	return s.err
}

// TestGrantWriteSurvivesLostResponse pins that a grant write which landed but
// reported failure — a GCS retry answered 412 after the first attempt
// committed, or a transport error after the commit — counts as done, instead
// of reading the advanced record as someone else's and revoking the family.
func TestGrantWriteSurvivesLostResponse(t *testing.T) {
	failures := map[string]error{
		"conflict":  sessionstore.ErrGrantConflict,
		"transport": errors.New("connection reset by peer"),
	}
	for failure, failErr := range failures {
		for name, store := range stores(t) {
			t.Run(failure+"/"+name, func(t *testing.T) {
				ctx := t.Context()
				now := time.Now()
				lost := func() sessionstore.Store { return &lostResponse{Store: store, err: failErr} }

				created, err := sessionstore.RedeemCode(ctx, lost(), testGrantFamily, now.Add(time.Hour))
				require.NoError(t, err)
				assert.True(t, created, "the code's own landed write is a redemption, not a reuse")

				result, err := sessionstore.RotateGrant(ctx, lost(), testGrantFamily, 0, now)
				require.NoError(t, err)
				assert.Equal(t, sessionstore.GrantRotated, result, "the rotation's own landed write is not a replay")

				result, err = sessionstore.RotateGrant(ctx, store, testGrantFamily, 1, now)
				require.NoError(t, err)
				assert.Equal(t, sessionstore.GrantRotated, result, "the family must survive the lost response")

				require.NoError(t, sessionstore.RevokeGrant(ctx, lost(), testGrantFamily))
				result, err = sessionstore.RotateGrant(ctx, store, testGrantFamily, 2, now)
				require.NoError(t, err)
				assert.Equal(t, sessionstore.GrantReplay, result, "a revocation whose response was lost still revokes")
			})
		}
	}
}

func TestGrantStoreExpiryContract(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			expiresAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			created, err := sessionstore.RedeemCode(ctx, store, testGrantFamily, expiresAt)
			require.NoError(t, err)
			require.True(t, created)

			// Expiry follows the caller's clock, not the backend's.
			result, err := sessionstore.RotateGrant(ctx, store, testGrantFamily, 0, expiresAt.Add(-time.Second))
			require.NoError(t, err)
			assert.Equal(t, sessionstore.GrantRotated, result)
			result, err = sessionstore.RotateGrant(ctx, store, testGrantFamily, 1, expiresAt)
			require.NoError(t, err)
			assert.Equal(t, sessionstore.GrantMissing, result, "a grant is expired from ExpiresAt on")

			// A record without an expiry is malformed and counts as expired.
			const zeroFamily = "00000000000000000000000000000001"
			created, err = sessionstore.RedeemCode(ctx, store, zeroFamily, time.Time{})
			require.NoError(t, err)
			require.True(t, created)
			result, err = sessionstore.RotateGrant(ctx, store, zeroFamily, 0, expiresAt)
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
			created, err := sessionstore.RedeemCode(ctx, store, testGrantFamily, now.Add(-time.Minute))
			require.NoError(t, err)
			require.True(t, created)
			created, err = sessionstore.RedeemCode(ctx, store, liveFamily, now.Add(time.Hour))
			require.NoError(t, err)
			require.True(t, created)
			require.NoError(t, store.SweepAuthState(ctx, now))

			_, version, err := store.LoadGrant(ctx, testGrantFamily)
			require.NoError(t, err)
			assert.Zero(t, version, "the expired grant must be swept")
			_, version, err = store.LoadGrant(ctx, liveFamily)
			require.NoError(t, err)
			assert.NotZero(t, version, "a live grant must survive the sweep")
		})
	}
}

func TestGrantStoreRejectsMalformedIdentity(t *testing.T) {
	for name, store := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			_, err := sessionstore.RedeemCode(ctx, store, "../not-a-family", time.Now().Add(time.Hour))
			require.ErrorIs(t, err, sessionstore.ErrInvalidSID)
			_, err = sessionstore.RotateGrant(ctx, store, "../not-a-family", 0, time.Now())
			require.ErrorIs(t, err, sessionstore.ErrInvalidSID)
			require.ErrorIs(t, sessionstore.RevokeGrant(ctx, store, "../not-a-family"), sessionstore.ErrInvalidSID)
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
