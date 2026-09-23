package sessionstore

import (
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

	_, err = RotateGrant(ctx, store, testGrantFamily, 0, time.Now())
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
