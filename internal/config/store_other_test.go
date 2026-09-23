//go:build !darwin

package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveConfigPathUsesXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	got, err := configPath()
	require.NoError(t, err)
	want := filepath.Join(os.Getenv("XDG_STATE_HOME"), "mcp-telegram", "config-v2")
	assert.Equal(t, want, got)
}

func TestResolveConfigPathRejectsRelativeStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative/path")

	_, err := configPath()
	require.Error(t, err)
}

func TestResolveConfigPathErrorsWhenMkdirAllFails(t *testing.T) {
	// Point XDG_STATE_HOME at a file (not a directory) so MkdirAll fails.
	f, err := os.CreateTemp(t.TempDir(), "notadir")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	t.Setenv("XDG_STATE_HOME", f.Name()) // file, not a dir → MkdirAll fails

	_, err = configPath()
	require.Error(t, err)
	// The state dir is created by the shared xdg.StateDir helper, whose error
	// names it the "state directory" (it backs both config.json and session.json).
	assert.Contains(t, err.Error(), "creating state directory")
}

func TestFileStoreGetSetDeleteList(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	s, err := NewStore()
	require.NoError(t, err)

	// Get on a missing key returns ErrNotFound.
	_, err = s.Get("missing")
	require.True(t, errors.Is(err, ErrNotFound))

	// Set then Get round-trips.
	require.NoError(t, s.Set("key1", "value1"))
	got, err := s.Get("key1")
	require.NoError(t, err)
	assert.Equal(t, "value1", got)

	// Set multiple keys; List returns all of them.
	require.NoError(t, s.Set("key2", "value2"))
	keys, err := s.List()
	require.NoError(t, err)
	assert.Equal(t, 2, len(keys))

	// Delete removes the key; subsequent Get returns ErrNotFound.
	require.NoError(t, s.Delete("key1"))
	_, err = s.Get("key1")
	require.True(t, errors.Is(err, ErrNotFound))

	// Deleting a non-existent key must not error.
	require.NoError(t, s.Delete("key1"))
}

func TestFileStoreSetOverwritesExistingValue(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	s, err := NewStore()
	require.NoError(t, err)

	require.NoError(t, s.Set("k", "original"))
	require.NoError(t, s.Set("k", "updated"))
	got, err := s.Get("k")
	require.NoError(t, err)
	assert.Equal(t, "updated", got)
}

func TestFileStoreIgnoresLegacyJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	// The removed aggregate format is never parsed or migrated.
	configFile := filepath.Join(dir, "mcp-telegram", "config.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(configFile), 0o700))
	require.NoError(t, os.WriteFile(configFile, []byte("not-valid-json{{{"), 0o600))

	s, storeErr := NewStore()
	require.NoError(t, storeErr)
	_, err := s.Get("any")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestFileStoreIndependentWritesDoNotLoseData(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	first, err := NewStore()
	require.NoError(t, err)
	second, err := NewStore()
	require.NoError(t, err)

	done := make(chan error, 2)
	go func() { done <- first.Set("first", "one") }()
	go func() { done <- second.Set("second", "two") }()
	require.NoError(t, <-done)
	require.NoError(t, <-done)
	keys, err := first.List()
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "second"}, keys)
	got, err := first.Get("second")
	require.NoError(t, err)
	assert.Equal(t, "two", got)
}
