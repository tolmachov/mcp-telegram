package sessionstore

import (
	"context"
	"io"
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
// with ErrGrantConflict after four rounds instead of spinning.
func TestUpdateGrantGivesUp(t *testing.T) {
	ctx := t.Context()
	inner := Encrypted(newTestFS(t), newCipher(t, testIssuer, newKey(t)))
	created, err := RedeemCode(ctx, inner, testGrantFamily, testSID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.True(t, created)

	store := &alwaysConflicting{Store: inner}
	_, err = RotateGrant(ctx, store, testGrantFamily, 0, time.Now())
	require.ErrorIs(t, err, ErrGrantConflict)
	assert.Equal(t, 4, store.loads)
	assert.Equal(t, 4, store.stores)
}
