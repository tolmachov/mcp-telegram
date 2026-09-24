package sessionstore

import (
	"net/http"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/fsouza/fake-gcs-server/fakestorage"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
)

// NewTestGCS returns a GCS store backed by an in-process emulator bucket.
func NewTestGCS(t *testing.T) *GCS {
	t.Helper()
	return newTestGCSWithTransport(t, func(rt http.RoundTripper) http.RoundTripper { return rt })
}

// newTestGCSWithTransport returns a GCS store backed by an in-process emulator
// bucket whose client talks to the emulator through wrap's round tripper, so
// a test can fault individual requests.
func newTestGCSWithTransport(t *testing.T, wrap func(http.RoundTripper) http.RoundTripper) *GCS {
	t.Helper()
	emulator, err := fakestorage.NewServerWithOptions(fakestorage.Options{NoListener: true})
	require.NoError(t, err)
	t.Cleanup(emulator.Stop)
	const bucket = "session-contract"
	emulator.CreateBucket(bucket) //nolint:staticcheck // Test helper has no result; a duplicate bucket panics internally.
	client, err := storage.NewClient(t.Context(),
		option.WithHTTPClient(&http.Client{Transport: wrap(emulator.HTTPClient().Transport)}),
		option.WithoutAuthentication())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return &GCS{bucket: client.Bucket(bucket)}
}
