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

// NewFS creates the directory (0700) if needed and returns the store. It also
// creates the revoked/ subdir that holds revocation tombstones.
func NewFS(dir string) (*FS, error) {
	if dir == "" {
		return nil, fmt.Errorf("sessionstore: directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("sessionstore: creating %s: %w", dir, err)
	}
	f := &FS{dir: dir}
	if err := os.MkdirAll(f.revokedDir(), 0o700); err != nil {
		return nil, fmt.Errorf("sessionstore: creating %s: %w", f.revokedDir(), err)
	}
	return f, nil
}

// path maps a session to its file. An empty sid keeps the legacy per-user
// layout. A non-empty sid is validated (ValidSID) before any store operation
// builds a path from a token-carried value, so it is always hex and cannot
// contain path separators or traversal sequences.
func (f *FS) path(userID tgid.UserID, sid string) string {
	return filepath.Join(f.dir, sessionBase(userID, sid))
}

// revokedDir is the subdir holding tombstones — the gotd client only writes
// session files in dir itself, so a tombstone here survives a blob re-store.
func (f *FS) revokedDir() string { return filepath.Join(f.dir, "revoked") }

func (f *FS) revokedPath(userID tgid.UserID, sid string) string {
	return filepath.Join(f.revokedDir(), sessionBase(userID, sid))
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

func (f *FS) List(_ context.Context) ([]SessionRef, error) {
	return f.listDir(f.dir)
}

func (f *FS) ListRevoked(_ context.Context) ([]SessionRef, error) {
	return f.listDir(f.revokedDir())
}

// listDir enumerates session-named files directly in dir (never recursing, so
// List skips the revoked/ subdir via IsDir), skipping foreign or unreadable
// entries rather than failing the whole listing.
func (f *FS) listDir(dir string) ([]SessionRef, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("sessionstore: listing %s: %w", dir, err)
	}
	var refs []SessionRef
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		userID, sid, ok := parseSessionBase(e.Name())
		if !ok {
			// A foreign file in the directory (temp file, stray note) — skip.
			continue
		}
		info, err := e.Info()
		if err != nil {
			// A transient stat error (the file vanished, or an EIO/ESTALE on one
			// entry) must not abort the whole listing — that would stall every
			// session's reclamation while one bad file persists. Skip it; the
			// next sweep retries.
			continue
		}
		refs = append(refs, SessionRef{UserID: userID, SID: sid, UpdatedAt: info.ModTime()})
	}
	return refs, nil
}

func (f *FS) Revoke(_ context.Context, userID tgid.UserID, sid string) error {
	// Tombstone first (source of truth), then remove the blob. A zero-byte file
	// is enough; its presence is the signal (checked via os.Stat, not read).
	p := f.revokedPath(userID, sid)
	if err := xdg.WriteFileAtomic(p, []byte{}, 0o600, ".revoked-*.tmp"); err != nil {
		return fmt.Errorf("sessionstore: writing tombstone %s: %w", p, err)
	}
	blob := f.path(userID, sid)
	if err := os.Remove(blob); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sessionstore: removing %s: %w", blob, err)
	}
	return nil
}

func (f *FS) Revoked(_ context.Context, userID tgid.UserID, sid string) (bool, error) {
	p := f.revokedPath(userID, sid)
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

func (f *FS) DeleteRevoked(_ context.Context, userID tgid.UserID, sid string) error {
	p := f.revokedPath(userID, sid)
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sessionstore: removing tombstone %s: %w", p, err)
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
