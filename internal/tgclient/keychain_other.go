//go:build !darwin

package tgclient

import (
	"context"
	"os"
	"path/filepath"

	"github.com/gotd/td/session"
)

// SessionStorage implements session.Storage using file storage on non-macOS platforms.
type SessionStorage struct {
	path string
}

// NewSessionStorage creates a new SessionStorage with file-based storage.
func NewSessionStorage() *SessionStorage {
	return &SessionStorage{
		path: getSessionPath(),
	}
}

func getSessionPath() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		homeDir, _ := os.UserHomeDir()
		stateHome = filepath.Join(homeDir, ".local", "state")
	}

	sessionDir := filepath.Join(stateHome, "mcp-telegram")
	_ = os.MkdirAll(sessionDir, 0o700)

	return filepath.Join(sessionDir, "session.json")
}

// LoadSession loads session data from file.
func (s *SessionStorage) LoadSession(_ context.Context) ([]byte, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, session.ErrNotFound
	}
	return data, nil
}

// StoreSession stores session data to file.
func (s *SessionStorage) StoreSession(_ context.Context, data []byte) error {
	return os.WriteFile(s.path, data, 0o600)
}

// DeleteSession removes session file.
func (s *SessionStorage) DeleteSession() error {
	err := os.Remove(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
