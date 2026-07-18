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

// FS stores one file per authorization under dir: <dir>/<userID>.<sid>.bin (or
// <dir>/<userID>.bin for legacy sessions with an empty sid). Intended for local
// development and self-hosted deployments with a persistent disk.
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

// path maps a session to its file. An empty sid keeps the legacy per-user
// layout. sid is server-generated hex (see authsrv.validSessionID), so it
// cannot contain path separators or traversal sequences.
func (f *FS) path(userID tgid.UserID, sid string) string {
	if sid == "" {
		return filepath.Join(f.dir, userID.String()+".bin")
	}
	return filepath.Join(f.dir, userID.String()+"."+sid+".bin")
}

func (f *FS) Session(userID tgid.UserID, sid string, _ []byte) session.Storage {
	return fsSession{path: f.path(userID, sid)}
}

func (f *FS) Exists(_ context.Context, userID tgid.UserID, sid string) (bool, error) {
	p := f.path(userID, sid)
	_, err := os.Stat(p)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("sessionstore: stat %s: %w", p, err)
	}
}

func (f *FS) Delete(_ context.Context, userID tgid.UserID, sid string) error {
	p := f.path(userID, sid)
	err := os.Remove(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sessionstore: removing %s: %w", p, err)
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
