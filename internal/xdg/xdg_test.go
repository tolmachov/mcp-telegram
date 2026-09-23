package xdg

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStateDirAndAtomicWrite(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	dir, err := StateDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(stateHome, "mcp-telegram"), dir)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	path := filepath.Join(dir, "value")
	require.NoError(t, WriteFileAtomic(path, []byte("first"), 0o600, ".value.*.tmp"))
	require.NoError(t, WriteFileAtomic(path, []byte("second"), 0o600, ".value.*.tmp"))
	got, err := os.ReadFile(path) //nolint:gosec // path is inside the test's private temporary directory.
	require.NoError(t, err)
	assert.Equal(t, "second", string(got))
	info, err = os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestStateDirRejectsRelativeHomeAndAtomicWriteNeedsParent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative")
	_, err := StateDir()
	assert.ErrorContains(t, err, "not absolute")

	err = WriteFileAtomic(filepath.Join(t.TempDir(), "missing", "value"), []byte("x"), 0o600, ".tmp-*")
	assert.ErrorContains(t, err, "creating temp file")
}

func TestStateDirTightensExistingPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions are not enforced on Windows")
	}
	stateHome := t.TempDir()
	dir := filepath.Join(stateHome, "mcp-telegram")
	require.NoError(t, os.Mkdir(dir, 0o750))
	t.Setenv("XDG_STATE_HOME", stateHome)
	_, err := StateDir()
	require.NoError(t, err)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestFileSessionDeleteSession(t *testing.T) {
	s := FileSession{Path: filepath.Join(t.TempDir(), "session.bin")}
	require.NoError(t, s.StoreSession(t.Context(), []byte("session")))
	require.NoError(t, s.DeleteSession())
	_, err := os.Stat(s.Path)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, s.DeleteSession(), "deleting a missing session is a no-op")
}
