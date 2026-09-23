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

	"github.com/tolmachov/mcp-telegram/internal/xdg"
)

func TestNewSessionStorageUsesXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	got, err := NewSessionStorage()
	require.NoError(t, err)
	want := filepath.Join(os.Getenv("XDG_STATE_HOME"), "mcp-telegram", "session-v2.bin")
	assert.Equal(t, want, got.Path)
}

func TestNewSessionStorageRejectsRelativeStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative/path")

	_, err := NewSessionStorage()
	require.Error(t, err)
}

func TestSessionStorageStoreSessionRoundTrips(t *testing.T) {
	target := filepath.Join(t.TempDir(), "session.json")
	s := &SessionStorage{xdg.FileSession{Path: target}}

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
	s := &SessionStorage{xdg.FileSession{Path: filepath.Join(t.TempDir(), "session.json")}}

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

	s := &SessionStorage{xdg.FileSession{Path: target}}
	_, err := s.LoadSession(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, session.ErrNotFound))
}

// TestDeleteSessionRemovesFileAndIdempotent verifies that DeleteSession removes
// the session file, and that a subsequent LoadSession returns ErrNotFound.
func TestDeleteSessionRemovesFileAndIdempotent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "session.json")
	s := &SessionStorage{xdg.FileSession{Path: target}}

	// Store a session first.
	require.NoError(t, s.StoreSession(context.Background(), []byte("data")))

	require.NoError(t, s.DeleteSession())

	_, err := s.LoadSession(context.Background())
	require.True(t, errors.Is(err, session.ErrNotFound))

	// Deleting again must not error (idempotent).
	require.NoError(t, s.DeleteSession())
}
