package sessionstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
	"github.com/tolmachov/mcp-telegram/internal/xdg"
)

// FS stores one file per user under dir: <dir>/<userID>.bin. Intended for
// local development and self-hosted deployments with a persistent disk.
type FS struct {
	dir string
}

// NewFS creates the directory (0700) if needed and returns the store.
func NewFS(dir string) (*FS, error) {
	if dir == "" {
		return nil, fmt.Errorf("sessionstore: directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("sessionstore: creating %s: %w", dir, err)
	}
	return &FS{dir: dir}, nil
}

func (f *FS) path(userID tgid.UserID) string {
	return filepath.Join(f.dir, userID.String()+".bin")
}

func (f *FS) Session(userID tgid.UserID) session.Storage {
	return fsSession{path: f.path(userID)}
}

func (f *FS) Exists(_ context.Context, userID tgid.UserID) (bool, error) {
	_, err := os.Stat(f.path(userID))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("sessionstore: stat %s: %w", f.path(userID), err)
	}
}

func (f *FS) Delete(_ context.Context, userID tgid.UserID) error {
	err := os.Remove(f.path(userID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sessionstore: removing %s: %w", f.path(userID), err)
	}
	return nil
}

type fsSession struct {
	path string
}

func (s fsSession) LoadSession(_ context.Context) ([]byte, error) {
	data, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, session.ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("sessionstore: reading %s: %w", s.path, err)
	case len(data) == 0:
		return nil, session.ErrNotFound
	}
	return data, nil
}

func (s fsSession) StoreSession(_ context.Context, data []byte) error {
	if err := xdg.WriteFileAtomic(s.path, data, 0o600, ".session-*.tmp"); err != nil {
		return fmt.Errorf("sessionstore: writing %s: %w", s.path, err)
	}
	return nil
}
