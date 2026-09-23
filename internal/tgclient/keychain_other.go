//go:build !darwin

package tgclient

import (
	"fmt"
	"path/filepath"

	"github.com/tolmachov/mcp-telegram/internal/xdg"
)

// SessionStorage implements session.Storage, and DeleteSession, using file
// storage on non-macOS platforms.
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
