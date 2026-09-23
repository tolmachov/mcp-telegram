//go:build !darwin

package tgclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tolmachov/mcp-telegram/internal/xdg"
)

// SessionStorage implements session.Storage using file storage on non-macOS platforms.
type SessionStorage struct {
	xdg.FileSession
}

// NewSessionStorage creates a new SessionStorage with file-based storage in the
// XDG state directory.
func NewSessionStorage() (*SessionStorage, error) {
	stateDir, err := xdg.StateDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine session storage location: %w; set XDG_STATE_HOME or ensure HOME is set", err)
	}
	return &SessionStorage{xdg.FileSession{Path: filepath.Join(stateDir, "session-v2.bin")}}, nil
}

// DeleteSession is idempotent: a missing file is success, since logout must
// succeed even if the session was never written (e.g. repeated logout calls
// from a stuck UI).
func (s *SessionStorage) DeleteSession() error {
	err := os.Remove(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting session file: %w", err)
	}
	return nil
}
