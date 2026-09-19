package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
	"github.com/tolmachov/mcp-telegram/internal/xdg"
)

// FS stores one file per authorization under dir/sessions-v3. Intended for local
// development and self-hosted deployments with a persistent disk.
type FS struct {
	dir   string
	locks sync.Map
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
	if err := os.MkdirAll(f.sessionsDir(), 0o700); err != nil {
		return nil, fmt.Errorf("sessionstore: creating %s: %w", f.sessionsDir(), err)
	}
	if err := os.MkdirAll(f.revokedDir(), 0o700); err != nil {
		return nil, fmt.Errorf("sessionstore: creating %s: %w", f.revokedDir(), err)
	}
	if err := os.MkdirAll(f.grantsDir(), 0o700); err != nil {
		return nil, fmt.Errorf("sessionstore: creating grant directory: %w", err)
	}
	return f, nil
}

// path maps a current-format session to its file.
func (f *FS) path(userID tgid.UserID, sid string) string {
	return filepath.Join(f.sessionsDir(), sessionBase(userID, sid))
}

// revokedDir is the subdir holding tombstones — the gotd client only writes
// session files in dir itself, so a tombstone here survives a blob re-store.
func (f *FS) sessionsDir() string { return filepath.Join(f.dir, "sessions-v3") }
func (f *FS) revokedDir() string  { return filepath.Join(f.dir, "revoked-v3") }
func (f *FS) grantsDir() string   { return filepath.Join(f.dir, "oauth-v3-grants") }

func (f *FS) grantPath(family string) string { return filepath.Join(f.grantsDir(), family+".json") }

func (f *FS) grantLock(family string) *sync.Mutex {
	lock, _ := f.locks.LoadOrStore(family, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (f *FS) revokedPath(userID tgid.UserID, sid string) string {
	return filepath.Join(f.revokedDir(), sessionBase(userID, sid))
}

func (f *FS) Session(userID tgid.UserID, sid string, _ []byte) session.Storage {
	if !ValidSID(sid) {
		return brokenSession{err: errInvalidStoreSID}
	}
	return fsSession{path: f.path(userID, sid)}
}

func (f *FS) Exists(_ context.Context, userID tgid.UserID, sid string) (bool, error) {
	if !ValidSID(sid) {
		return false, errInvalidStoreSID
	}
	p := f.path(userID, sid)
	info, err := os.Stat(p)
	switch {
	case err == nil:
		// A 0-byte session file is treated as absent to match LoadSession (which
		// maps an empty read to session.ErrNotFound) and the GCS backend (which
		// probes attrs.Size > 0). Both backends must agree so the refresh Exists
		// gate never passes a session the client build would then reject.
		return info.Size() > 0, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("sessionstore: stat %s: %w", p, err)
	}
}

func (f *FS) Delete(_ context.Context, userID tgid.UserID, sid string) error {
	if !ValidSID(sid) {
		return errInvalidStoreSID
	}
	p := f.path(userID, sid)
	err := os.Remove(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sessionstore: removing %s: %w", p, err)
	}
	return nil
}

func (f *FS) List(_ context.Context) ([]SessionRef, error) {
	return f.listDir(f.sessionsDir())
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
	if !ValidSID(sid) {
		return errInvalidStoreSID
	}
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
	if !ValidSID(sid) {
		return false, errInvalidStoreSID
	}
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
	if !ValidSID(sid) {
		return errInvalidStoreSID
	}
	p := f.revokedPath(userID, sid)
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sessionstore: removing tombstone %s: %w", p, err)
	}
	return nil
}

func (f *FS) RedeemCode(_ context.Context, family, sid string, expiresAt time.Time) (bool, error) {
	if !ValidSID(family) || !ValidSID(sid) {
		return false, errInvalidStoreSID
	}
	lock := f.grantLock(family)
	lock.Lock()
	defer lock.Unlock()
	path := f.grantPath(family)
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("sessionstore: probing grant: %w", err)
	}
	data, err := json.Marshal(grantRecord{SID: sid, ExpiresAt: expiresAt})
	if err != nil {
		return false, fmt.Errorf("sessionstore: encoding grant: %w", err)
	}
	if err := xdg.WriteFileAtomic(path, data, 0o600, ".grant-*.tmp"); err != nil {
		return false, fmt.Errorf("sessionstore: creating grant: %w", err)
	}
	return true, nil
}

func (f *FS) RotateGrant(_ context.Context, family string, generation int64) (GrantRotation, error) {
	if !ValidSID(family) {
		return GrantMissing, errInvalidStoreSID
	}
	lock := f.grantLock(family)
	lock.Lock()
	defer lock.Unlock()
	path := f.grantPath(family)
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from a validated fixed-length hex family
	if errors.Is(err, os.ErrNotExist) {
		return GrantMissing, nil
	}
	if err != nil {
		return GrantMissing, fmt.Errorf("sessionstore: reading grant: %w", err)
	}
	var grant grantRecord
	if err := json.Unmarshal(data, &grant); err != nil {
		return GrantMissing, fmt.Errorf("sessionstore: parsing grant: %w", err)
	}
	result := GrantRotated
	if !time.Now().Before(grant.ExpiresAt) {
		return GrantMissing, nil
	}
	if grant.Revoked || grant.Generation != generation {
		grant.Revoked = true
		result = GrantReplay
	} else {
		grant.Generation++
	}
	data, err = json.Marshal(grant)
	if err != nil {
		return GrantMissing, fmt.Errorf("sessionstore: encoding rotated grant: %w", err)
	}
	if err := xdg.WriteFileAtomic(path, data, 0o600, ".grant-*.tmp"); err != nil {
		return GrantMissing, fmt.Errorf("sessionstore: rotating grant: %w", err)
	}
	return result, nil
}

func (f *FS) RevokeGrant(_ context.Context, family string) error {
	if !ValidSID(family) {
		return errInvalidStoreSID
	}
	lock := f.grantLock(family)
	lock.Lock()
	defer lock.Unlock()
	path := f.grantPath(family)
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from a validated fixed-length hex family
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sessionstore: reading grant for revoke: %w", err)
	}
	var grant grantRecord
	if err := json.Unmarshal(data, &grant); err != nil {
		return fmt.Errorf("sessionstore: parsing grant for revoke: %w", err)
	}
	grant.Revoked = true
	data, _ = json.Marshal(grant)
	return xdg.WriteFileAtomic(path, data, 0o600, ".grant-*.tmp")
}

func (f *FS) SweepAuthState(_ context.Context, now time.Time) error {
	entries, err := os.ReadDir(f.grantsDir())
	if err != nil {
		return fmt.Errorf("sessionstore: listing grants: %w", err)
	}
	for _, entry := range entries {
		family, ok := strings.CutSuffix(entry.Name(), ".json")
		if entry.IsDir() || !ok || !ValidSID(family) {
			continue
		}
		lock := f.grantLock(family)
		lock.Lock()
		data, readErr := os.ReadFile(f.grantPath(family))
		var grant grantRecord
		if readErr == nil {
			readErr = json.Unmarshal(data, &grant)
		}
		if readErr == nil && !now.Before(grant.ExpiresAt) {
			readErr = os.Remove(f.grantPath(family))
		}
		lock.Unlock()
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return fmt.Errorf("sessionstore: sweeping grant %s: %w", family, readErr)
		}
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
