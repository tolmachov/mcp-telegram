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
	f := &FS{dir: dir}
	for _, d := range []string{f.sessionsDir(), f.revokedDir(), f.grantsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("sessionstore: creating %s: %w", d, err)
		}
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

// loadGrant reads family's grant record; found is false when none exists. The
// caller holds the family's grantLock.
func (f *FS) loadGrant(family string) (grant grantRecord, found bool, err error) {
	data, err := os.ReadFile(f.grantPath(family)) //nolint:gosec // path is derived from a validated fixed-length hex family
	if errors.Is(err, os.ErrNotExist) {
		return grantRecord{}, false, nil
	}
	if err != nil {
		return grantRecord{}, false, fmt.Errorf("sessionstore: reading grant: %w", err)
	}
	if err := json.Unmarshal(data, &grant); err != nil {
		return grantRecord{}, false, fmt.Errorf("sessionstore: parsing grant: %w", err)
	}
	return grant, true, nil
}

// storeGrant atomically replaces family's grant record. The caller holds the
// family's grantLock.
func (f *FS) storeGrant(family string, grant grantRecord) error {
	data, err := json.Marshal(grant)
	if err != nil {
		return fmt.Errorf("sessionstore: encoding grant: %w", err)
	}
	if err := xdg.WriteFileAtomic(f.grantPath(family), data, 0o600, ".grant-*.tmp"); err != nil {
		return fmt.Errorf("sessionstore: writing grant: %w", err)
	}
	return nil
}

func (f *FS) grantLock(family string) *sync.Mutex {
	lock, _ := f.locks.LoadOrStore(family, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (f *FS) revokedPath(userID tgid.UserID, sid string) string {
	return filepath.Join(f.revokedDir(), sessionBase(userID, sid))
}

func (f *FS) Session(userID tgid.UserID, sid string, _ []byte) session.Storage {
	if !ValidSID(sid) {
		return brokenSession{err: ErrInvalidSID}
	}
	return xdg.FileSession{Path: f.path(userID, sid)}
}

func (f *FS) Exists(_ context.Context, userID tgid.UserID, sid string) (bool, error) {
	if !ValidSID(sid) {
		return false, ErrInvalidSID
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
		return ErrInvalidSID
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

func (f *FS) Revoke(ctx context.Context, userID tgid.UserID, sid string) error {
	if !ValidSID(sid) {
		return ErrInvalidSID
	}
	// Tombstone first (source of truth), then remove the blob. A zero-byte file
	// is enough; its presence is the signal (checked via os.Stat, not read).
	p := f.revokedPath(userID, sid)
	if err := xdg.WriteFileAtomic(p, []byte{}, 0o600, ".revoked-*.tmp"); err != nil {
		return fmt.Errorf("sessionstore: writing tombstone %s: %w", p, err)
	}
	return f.Delete(ctx, userID, sid)
}

func (f *FS) Revoked(_ context.Context, userID tgid.UserID, sid string) (bool, error) {
	if !ValidSID(sid) {
		return false, ErrInvalidSID
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
		return ErrInvalidSID
	}
	p := f.revokedPath(userID, sid)
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sessionstore: removing tombstone %s: %w", p, err)
	}
	return nil
}

func (f *FS) RedeemCode(_ context.Context, family, sid string, expiresAt time.Time) (bool, error) {
	if !ValidSID(family) || !ValidSID(sid) {
		return false, ErrInvalidSID
	}
	lock := f.grantLock(family)
	lock.Lock()
	defer lock.Unlock()
	if _, err := os.Stat(f.grantPath(family)); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("sessionstore: probing grant: %w", err)
	}
	if err := f.storeGrant(family, grantRecord{SID: sid, ExpiresAt: expiresAt}); err != nil {
		return false, err
	}
	return true, nil
}

func (f *FS) RotateGrant(_ context.Context, family string, generation int64) (GrantRotation, error) {
	if !ValidSID(family) {
		return GrantMissing, ErrInvalidSID
	}
	lock := f.grantLock(family)
	lock.Lock()
	defer lock.Unlock()
	grant, found, err := f.loadGrant(family)
	if err != nil || !found {
		return GrantMissing, err
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
	if err := f.storeGrant(family, grant); err != nil {
		return GrantMissing, err
	}
	return result, nil
}

func (f *FS) RevokeGrant(_ context.Context, family string) error {
	if !ValidSID(family) {
		return ErrInvalidSID
	}
	lock := f.grantLock(family)
	lock.Lock()
	defer lock.Unlock()
	grant, found, err := f.loadGrant(family)
	if err != nil || !found {
		return err
	}
	grant.Revoked = true
	return f.storeGrant(family, grant)
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
		grant, found, err := f.loadGrant(family)
		if err == nil && found && !now.Before(grant.ExpiresAt) {
			err = os.Remove(f.grantPath(family))
		}
		lock.Unlock()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("sessionstore: sweeping grant %s: %w", family, err)
		}
	}
	return nil
}
