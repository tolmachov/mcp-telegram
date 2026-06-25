//go:build !darwin

package tgclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gotd/td/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSessionPathUsesXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	got, err := resolveSessionPath()
	require.NoError(t, err)
	want := filepath.Join(os.Getenv("XDG_STATE_HOME"), "mcp-telegram", "session.json")
	assert.Equal(t, want, got)
}

func TestResolveSessionPathRejectsRelativeStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative/path")

	_, err := resolveSessionPath()
	require.Error(t, err)
}

func TestSessionStorageStoreSessionCreatesDirAndRoundTrips(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "nested", "session.json")
	s := &SessionStorage{path: target}

	want := []byte("session-data")
	require.NoError(t, s.StoreSession(context.Background(), want))

	got, err := s.LoadSession(context.Background())
	require.NoError(t, err)
	assert.Equal(t, want, got)

	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// TestLoadSessionReturnsNotFoundWhenMissing verifies that LoadSession returns
// session.ErrNotFound (not a raw os error) when no session file exists.
func TestLoadSessionReturnsNotFoundWhenMissing(t *testing.T) {
	s := &SessionStorage{path: filepath.Join(t.TempDir(), "session.json")}

	_, err := s.LoadSession(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, session.ErrNotFound))
}

// TestLoadSessionReturnsNotFoundForEmptyFile verifies that a zero-byte session
// file is treated as absent rather than silently returning an empty session,
// which would cause unexpected re-authentication and potential flood-wait errors.
func TestLoadSessionReturnsNotFoundForEmptyFile(t *testing.T) {
	target := filepath.Join(t.TempDir(), "session.json")
	require.NoError(t, os.WriteFile(target, []byte{}, 0o600))

	s := &SessionStorage{path: target}
	_, err := s.LoadSession(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, session.ErrNotFound))
}

// TestDeleteSessionRemovesFileAndIdempotent verifies that DeleteSession removes
// the session file, and that a subsequent LoadSession returns ErrNotFound.
func TestDeleteSessionRemovesFileAndIdempotent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "session.json")
	s := &SessionStorage{path: target}

	// Store a session first.
	require.NoError(t, s.StoreSession(context.Background(), []byte("data")))

	require.NoError(t, s.DeleteSession())

	_, err := s.LoadSession(context.Background())
	require.True(t, errors.Is(err, session.ErrNotFound))

	// Deleting again must not error (idempotent).
	require.NoError(t, s.DeleteSession())
}
