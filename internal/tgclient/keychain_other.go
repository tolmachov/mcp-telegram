//go:build !darwin

package tgclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// LoadSession loads session data from file.
func (s *SessionStorage) LoadSession(_ context.Context) ([]byte, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
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

// StoreSession atomically writes session data to file using a temp-file rename,
// ensuring the previous session is never left in a partial state.
func (s *SessionStorage) StoreSession(_ context.Context, data []byte) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating session directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".session.*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp session file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		// os.ErrNotExist is expected on the success path (rename consumed the file).
		if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Debug("cleanup of temp session file failed", "path", tmpPath, "err", err)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		if cerr := tmp.Close(); cerr != nil {
			slog.Debug("closing temp session file after chmod failure", "error", cerr)
		}
		return fmt.Errorf("setting temp session file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		if cerr := tmp.Close(); cerr != nil {
			slog.Debug("closing temp session file after write failure", "error", cerr)
		}
		return fmt.Errorf("writing temp session file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		if cerr := tmp.Close(); cerr != nil {
			slog.Debug("closing temp session file after sync failure", "error", cerr)
		}
		return fmt.Errorf("syncing temp session file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp session file: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("renaming temp session file into place: %w", err)
	}
	return nil
}

// DeleteSession removes session file.
func (s *SessionStorage) DeleteSession() error {
	err := os.Remove(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
