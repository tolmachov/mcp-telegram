//go:build darwin

package secret

import (
	"errors"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBackend is an in-memory stand-in for the keychain. load and write
// counters let tests assert that the cache (not the backend) serves reads
// after the initial load.
type fakeBackend struct {
	cfg     map[string]string
	sess    []byte
	loadErr error
	loads   int
	writes  int
}

func (f *fakeBackend) load() (map[string]string, []byte, error) {
	f.loads++
	if f.loadErr != nil {
		return nil, nil, f.loadErr
	}
	// Return a defensive copy so the vault's internal state can't feed back
	// into the fake's storage via shared references.
	out := make(map[string]string, len(f.cfg))
	maps.Copy(out, f.cfg)
	var sess []byte
	if len(f.sess) > 0 {
		sess = append([]byte(nil), f.sess...)
	}
	return out, sess, nil
}

func (f *fakeBackend) write(cfg map[string]string, sess []byte) error {
	f.writes++
	// Mirror the real writeBlob contract: empty inputs imply DeleteItem, which
	// from the fake's perspective means "clear everything".
	if len(cfg) == 0 && len(sess) == 0 {
		f.cfg = nil
		f.sess = nil
		return nil
	}
	f.cfg = make(map[string]string, len(cfg))
	maps.Copy(f.cfg, cfg)
	if len(sess) > 0 {
		f.sess = append([]byte(nil), sess...)
	} else {
		f.sess = nil
	}
	return nil
}

func newTestVault(b *fakeBackend) *Vault {
	return &Vault{loadFn: b.load, writeFn: b.write}
}

func TestVaultEnsureLoadedRunsOnce(t *testing.T) {
	b := &fakeBackend{cfg: map[string]string{"A": "1"}}
	v := newTestVault(b)

	_, err := v.ConfigGet("A")
	require.NoError(t, err)
	_, err = v.ConfigGet("A")
	require.NoError(t, err)
	_, err = v.ConfigList()
	require.NoError(t, err)

	assert.Equal(t, 1, b.loads, "the backend must be read exactly once — the rationale for Vault is a single Keychain prompt")
}

func TestVaultConfigGetNotFound(t *testing.T) {
	b := &fakeBackend{}
	v := newTestVault(b)

	_, err := v.ConfigGet("missing")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
}

func TestVaultConfigSetAndList(t *testing.T) {
	b := &fakeBackend{}
	v := newTestVault(b)

	require.NoError(t, v.ConfigSet("FOO", "one"))
	require.NoError(t, v.ConfigSet("BAR", "two"))

	got, err := v.ConfigGet("FOO")
	require.NoError(t, err)
	assert.Equal(t, "one", got)

	keys, err := v.ConfigList()
	require.NoError(t, err)
	assert.Equal(t, []string{"BAR", "FOO"}, keys, "List must be sorted so CLI output is stable")
}

func TestVaultCacheUpdatedOnlyAfterWriteSuccess(t *testing.T) {
	b := &fakeBackend{}
	v := newTestVault(b)

	// Swap writeFn to fail once, verify the cache wasn't mutated, then allow
	// the second write to succeed. This protects against a regression where
	// ConfigSet updates v.config before writeBlob returns.
	failErr := errors.New("simulated keychain write failure")
	v.writeFn = func(map[string]string, []byte) error { return failErr }

	err := v.ConfigSet("A", "one")
	require.ErrorIs(t, err, failErr)

	// Reading should still see an empty vault (no local mutation leaked).
	_, err = v.ConfigGet("A")
	assert.True(t, errors.Is(err, ErrNotFound), "cache must not reflect a failed write")

	// Restore the real fake writer and retry.
	v.writeFn = b.write
	require.NoError(t, v.ConfigSet("A", "one"))
	got, err := v.ConfigGet("A")
	require.NoError(t, err)
	assert.Equal(t, "one", got)
}

func TestVaultConfigDeleteEmptyTriggersBackendDelete(t *testing.T) {
	b := &fakeBackend{cfg: map[string]string{"A": "1"}}
	v := newTestVault(b)

	require.NoError(t, v.ConfigDelete("A"))

	// An entirely empty vault (no config, no session) must trigger the write
	// path with empty inputs, which in the real backend maps to DeleteItem.
	assert.Empty(t, b.cfg)
	assert.Empty(t, b.sess)
	assert.Equal(t, 1, b.writes)
}

func TestVaultConfigDeleteMissingIsNoOp(t *testing.T) {
	b := &fakeBackend{cfg: map[string]string{"A": "1"}}
	v := newTestVault(b)

	require.NoError(t, v.ConfigDelete("does-not-exist"))
	assert.Equal(t, 0, b.writes, "deleting a missing key must not write back to the backend")
}

func TestVaultSessionRoundTrip(t *testing.T) {
	b := &fakeBackend{}
	v := newTestVault(b)

	_, err := v.SessionLoad()
	require.ErrorIs(t, err, ErrNotFound)

	want := []byte("session-bytes")
	require.NoError(t, v.SessionStore(want))

	got, err := v.SessionLoad()
	require.NoError(t, err)
	assert.Equal(t, want, got)

	// SessionLoad must hand back a copy — mutating the return value must not
	// corrupt the stored session.
	got[0] = '!'
	got2, err := v.SessionLoad()
	require.NoError(t, err)
	assert.Equal(t, want, got2, "SessionLoad leaked a shared slice; callers could mutate the cache")
}

func TestVaultSessionDeleteIdempotent(t *testing.T) {
	b := &fakeBackend{}
	v := newTestVault(b)

	// First delete with no session present: short-circuits, no write.
	require.NoError(t, v.SessionDelete())
	assert.Equal(t, 0, b.writes)

	require.NoError(t, v.SessionStore([]byte("x")))
	writesAfterStore := b.writes
	require.NoError(t, v.SessionDelete())
	assert.Equal(t, writesAfterStore+1, b.writes)

	// Deleting again must be a no-op.
	require.NoError(t, v.SessionDelete())
	assert.Equal(t, writesAfterStore+1, b.writes)
}

func TestVaultConfigLoadAllReturnsCopy(t *testing.T) {
	b := &fakeBackend{cfg: map[string]string{"A": "1"}}
	v := newTestVault(b)

	snapshot, err := v.ConfigLoadAll()
	require.NoError(t, err)
	snapshot["A"] = "mutated"

	got, err := v.ConfigGet("A")
	require.NoError(t, err)
	assert.Equal(t, "1", got, "ConfigLoadAll must return a defensive copy so callers can't mutate the cache")
}

func TestVaultSurfacesLoadError(t *testing.T) {
	want := errors.New("keychain unavailable")
	b := &fakeBackend{loadErr: want}
	v := newTestVault(b)

	_, err := v.ConfigGet("A")
	require.ErrorIs(t, err, want)

	// A second call must retry the backend (loaded flag stays false on error),
	// otherwise a transient keychain lock would poison the entire process.
	_, err = v.ConfigGet("A")
	require.ErrorIs(t, err, want)
	assert.Equal(t, 2, b.loads)
}
