package sessionstore

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testGrantFamily = "fedcba9876543210fedcba9876543210"

func TestGCSGrantCorruptionFailsWithoutOverwrite(t *testing.T) {
	store := NewTestGCS(t)
	ctx := t.Context()
	object := store.bucket.Object(grantObjectName(testGrantFamily))
	w := object.NewWriter(ctx)
	_, err := io.WriteString(w, "not-json")
	require.NoError(t, err)
	require.NoError(t, w.Close())

	_, err = RotateGrant(ctx, Encrypted(store, newCipher(t, testIssuer, newKey(t))), testGrantFamily, 0, time.Now())
	require.ErrorContains(t, err, "parsing grant")
	r, err := object.NewReader(ctx)
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	assert.Equal(t, "not-json", string(data), "a failed read must not rewrite authorization state")
}

// TestGrantLoadsPersistedRecord pins the persisted grant JSON: a record written
// by an earlier build, including the session id it used to carry, still loads.
func TestGrantLoadsPersistedRecord(t *testing.T) {
	const persisted = `{"sid":"0123456789abcdef0123456789abcdef","generation":2,"expires_at":"2026-01-02T03:04:05Z","revoked":true,"write_id":"W"}`
	want := GrantRecord{Generation: 2, ExpiresAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Revoked: true, WriteID: "W"}

	fs := newTestFS(t)
	require.NoError(t, os.WriteFile(fs.grantPath(testGrantFamily), []byte(persisted), 0o600))
	gcs := NewTestGCS(t)
	w := gcs.bucket.Object(grantObjectName(testGrantFamily)).NewWriter(t.Context())
	_, err := io.WriteString(w, persisted)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	for name, b := range map[string]backend{"fs": fs, "gcs": gcs} {
		got, version, err := b.LoadGrant(t.Context(), testGrantFamily)
		require.NoError(t, err, name)
		assert.NotZero(t, version, name)
		assert.Equal(t, want, got, name)
	}
}

// TestGrantRotationZeroIsRefusal pins that an outcome nobody set cannot pass
// the token endpoint's success check.
func TestGrantRotationZeroIsRefusal(t *testing.T) {
	var unset GrantRotation
	assert.NotEqual(t, GrantRotated, unset)
}

// alwaysConflicting loses every grant write to a concurrent writer.
type alwaysConflicting struct {
	Store
	loads, stores int
}

func (s *alwaysConflicting) LoadGrant(ctx context.Context, family string) (GrantRecord, int64, error) {
	s.loads++
	return s.Store.LoadGrant(ctx, family)
}

func (s *alwaysConflicting) StoreGrant(context.Context, string, GrantRecord, int64) error {
	s.stores++
	return ErrGrantConflict
}

// TestUpdateGrantGivesUp pins that a grant under constant contention fails
// with ErrGrantConflict after four rounds instead of spinning. Each round
// loads once to compute the write and once to check whether the refused write
// landed after all.
func TestUpdateGrantGivesUp(t *testing.T) {
	ctx := t.Context()
	inner := Encrypted(newTestFS(t), newCipher(t, testIssuer, newKey(t)))
	created, err := RedeemCode(ctx, inner, testGrantFamily, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.True(t, created)

	store := &alwaysConflicting{Store: inner}
	_, err = RotateGrant(ctx, store, testGrantFamily, 0, time.Now())
	require.ErrorIs(t, err, ErrGrantConflict)
	assert.Equal(t, 8, store.loads)
	assert.Equal(t, 4, store.stores)
}

// unreadableAfterWrite fails a grant write, and every read after it, with
// its own errors.
type unreadableAfterWrite struct {
	Store
	storeErr, loadErr error
	wrote             bool
}

func (s *unreadableAfterWrite) LoadGrant(ctx context.Context, family string) (GrantRecord, int64, error) {
	if s.wrote {
		return GrantRecord{}, 0, s.loadErr
	}
	return s.Store.LoadGrant(ctx, family)
}

func (s *unreadableAfterWrite) StoreGrant(context.Context, string, GrantRecord, int64) error {
	s.wrote = true
	return s.storeErr
}

// TestWriteGrantReportsAnUnknownOutcome pins that a failed grant write whose
// re-read fails too reports both errors — the write may have landed — rather
// than only the write's, or a lost race to retry.
func TestWriteGrantReportsAnUnknownOutcome(t *testing.T) {
	ctx := t.Context()
	inner := Encrypted(newTestFS(t), newCipher(t, testIssuer, newKey(t)))
	created, err := RedeemCode(ctx, inner, testGrantFamily, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.True(t, created)

	for _, storeErr := range []error{ErrGrantConflict, errors.New("connection reset by peer")} {
		store := &unreadableAfterWrite{Store: inner, storeErr: storeErr, loadErr: errors.New("bucket unavailable")}
		_, err := RotateGrant(ctx, store, testGrantFamily, 0, time.Now())
		require.ErrorIs(t, err, storeErr)
		require.ErrorIs(t, err, store.loadErr)
		assert.ErrorContains(t, err, "outcome unknown")
	}
}
