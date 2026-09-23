package sessionstore

import (
	"testing"

	"github.com/fsouza/fake-gcs-server/fakestorage"
	"github.com/stretchr/testify/require"
)

// NewTestGCS returns a GCS store backed by an in-process emulator bucket.
func NewTestGCS(t *testing.T) *GCS {
	t.Helper()
	emulator, err := fakestorage.NewServerWithOptions(fakestorage.Options{NoListener: true})
	require.NoError(t, err)
	t.Cleanup(emulator.Stop)
	const bucket = "session-contract"
	emulator.CreateBucket(bucket) //nolint:staticcheck // Test helper has no result; a duplicate bucket panics internally.
	return &GCS{bucket: emulator.Client().Bucket(bucket)}
}
