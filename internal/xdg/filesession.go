package xdg

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/gotd/td/session"
)

// FileSession is a gotd session.Storage kept in one file. It backs the local
// single-account session on non-darwin platforms and the per-user sessions of
// the directory-backed HTTP store. The parent directory must already exist.
type FileSession struct {
	Path string
}

// LoadSession reads the persisted session, treating "file missing" and
// "file present but empty" as the same condition (session.ErrNotFound).
// An empty file would otherwise look like "logged in with no session",
// which triggers a silent re-auth and, under load, a FLOOD_WAIT.
func (s FileSession) LoadSession(_ context.Context) ([]byte, error) {
	data, err := os.ReadFile(s.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, session.ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("reading session file %s: %w", s.Path, err)
	case len(data) == 0:
		return nil, session.ErrNotFound
	}
	return data, nil
}

// StoreSession writes the session atomically (see WriteFileAtomic), so the
// previous session is never left in a partial state.
func (s FileSession) StoreSession(_ context.Context, data []byte) error {
	if err := WriteFileAtomic(s.Path, data, 0o600, ".session-*.tmp"); err != nil {
		return fmt.Errorf("writing session file %s: %w", s.Path, err)
	}
	return nil
}
