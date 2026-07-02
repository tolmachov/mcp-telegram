//go:build !darwin

package tgclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/xdg"
)

// SessionStorage implements session.Storage using file storage on non-macOS platforms.
type SessionStorage struct {
	path string
}

// NewSessionStorage creates a new SessionStorage with file-based storage.
func NewSessionStorage() (*SessionStorage, error) {
	path, err := getSessionPath()
	if err != nil {
		return nil, err
	}
	return &SessionStorage{path: path}, nil
}

func getSessionPath() (string, error) {
	path, err := resolveSessionPath()
	if err != nil {
		return "", fmt.Errorf("cannot determine session storage location: %w; set XDG_STATE_HOME or ensure HOME is set", err)
	}
	return path, nil
}

func resolveSessionPath() (string, error) {
	stateDir, err := xdg.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "session.json"), nil
}

// LoadSession reads the persisted session, treating "file missing" and
// "file present but empty" as the same condition (session.ErrNotFound).
// An empty file would otherwise look like "logged in with no session",
// which triggers a silent re-auth and, under load, a FLOOD_WAIT.
func (s *SessionStorage) LoadSession(_ context.Context) ([]byte, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading session file: %w", err)
	}
	if len(data) == 0 {
		return nil, session.ErrNotFound
	}
	return data, nil
}

// StoreSession atomically writes session data to file using a temp-file rename,
// ensuring the previous session is never left in a partial state.
func (s *SessionStorage) StoreSession(_ context.Context, data []byte) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating session directory: %w", err)
	}
	if err := xdg.WriteFileAtomic(s.path, data, 0o600, ".session.*.json.tmp"); err != nil {
		return fmt.Errorf("storing session: %w", err)
	}
	return nil
}

// DeleteSession is idempotent: a missing file is success, since logout must
// succeed even if the session was never written (e.g. repeated logout calls
// from a stuck UI).
func (s *SessionStorage) DeleteSession() error {
	err := os.Remove(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting session file: %w", err)
	}
	return nil
}
