package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
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
	dir string
	// grantLocks make a grant's compare and write one step. Families hash onto a
	// fixed set of stripes, so the lock set stays bounded however many
	// families come and go; two families sharing a stripe merely queue.
	grantLocks [grantLockStripes]sync.Mutex
}

// grantLockStripes is the number of grant locks per FS store.
const grantLockStripes = 64

// NewFS creates the directory and its sessions, tombstone and grant
// subdirectories (0700) if needed and returns the store.
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

// Sessions, tombstones and grants each live in their own subdirectory. The
// gotd client only writes session files under sessionsDir, so a tombstone in
// revokedDir survives a blob re-store.
func (f *FS) sessionsDir() string { return filepath.Join(f.dir, "sessions-v3") }
func (f *FS) revokedDir() string  { return filepath.Join(f.dir, "revoked-v3") }
func (f *FS) grantsDir() string   { return filepath.Join(f.dir, "oauth-v3-grants") }

func (f *FS) grantPath(family string) string { return filepath.Join(f.grantsDir(), family+".json") }

// LoadGrant reads family's grant record. The version is a hash of the file
// contents, so StoreGrant compares the record's value: an equal hash means
// the stored record is the one the caller read, which is all a
// compare-and-swap needs.
func (f *FS) LoadGrant(_ context.Context, family string) (GrantRecord, int64, error) {
	data, err := os.ReadFile(f.grantPath(family)) //nolint:gosec // Encrypted validated family as fixed-length hex
	if errors.Is(err, os.ErrNotExist) {
		return GrantRecord{}, 0, nil
	}
	if err != nil {
		return GrantRecord{}, 0, fmt.Errorf("sessionstore: reading grant: %w", err)
	}
	var grant GrantRecord
	if err := json.Unmarshal(data, &grant); err != nil {
		return GrantRecord{}, 0, fmt.Errorf("%w: %w", errUndecodableGrant, err)
	}
	h := fnv.New64a()
	_, _ = h.Write(data)
	return grant, int64(h.Sum64() | 1), nil //nolint:gosec // the version is an opaque bit pattern; overflow is irrelevant
}

// StoreGrant atomically replaces family's grant record if its version still
// matches. Holding family's lock stripe makes the compare and the write one
// step.
func (f *FS) StoreGrant(ctx context.Context, family string, grant GrantRecord, version int64) error {
	lock := f.grantLock(family)
	lock.Lock()
	defer lock.Unlock()
	_, current, err := f.LoadGrant(ctx, family)
	if err != nil {
		return err
	}
	if current != version {
		return ErrGrantConflict
	}
	data, err := json.Marshal(grant)
	if err != nil {
		return fmt.Errorf("sessionstore: encoding grant: %w", err)
	}
	if err := xdg.WriteFileAtomic(f.grantPath(family), data, 0o600, ".grant-*.tmp"); err != nil {
		return fmt.Errorf("sessionstore: writing grant: %w", err)
	}
	return nil
}

// grantLock returns the lock stripe family hashes onto.
func (f *FS) grantLock(family string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(family))
	return &f.grantLocks[h.Sum32()%grantLockStripes]
}

func (f *FS) revokedPath(userID tgid.UserID, sid string) string {
	return filepath.Join(f.revokedDir(), sessionBase(userID, sid))
}

func (f *FS) Session(userID tgid.UserID, sid string) session.Storage {
	return xdg.FileSession{Path: f.path(userID, sid)}
}

func (f *FS) Exists(_ context.Context, userID tgid.UserID, sid string) (bool, error) {
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
	if err := (xdg.FileSession{Path: f.path(userID, sid)}).DeleteSession(); err != nil {
		return fmt.Errorf("sessionstore: %w", err)
	}
	return nil
}

func (f *FS) List(_ context.Context) ([]SessionRef, error) {
	return f.listDir(f.sessionsDir())
}

func (f *FS) ListRevoked(_ context.Context) ([]SessionRef, error) {
	return f.listDir(f.revokedDir())
}

// listDir enumerates session-named files directly in dir, skipping
// subdirectories and foreign or unreadable entries rather than failing the
// whole listing.
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
	// Tombstone first (source of truth), then remove the blob. A zero-byte file
	// is enough; its presence is the signal (checked via os.Stat, not read).
	p := f.revokedPath(userID, sid)
	if err := xdg.WriteFileAtomic(p, []byte{}, 0o600, ".revoked-*.tmp"); err != nil {
		return fmt.Errorf("sessionstore: writing tombstone %s: %w", p, err)
	}
	return f.Delete(ctx, userID, sid)
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

// SweepAuthState implements Store.SweepAuthState.
func (f *FS) SweepAuthState(ctx context.Context, now time.Time) ([]string, error) {
	entries, err := os.ReadDir(f.grantsDir())
	if err != nil {
		return nil, fmt.Errorf("sessionstore: listing grants: %w", err)
	}
	var undecodable []string
	var errs []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return undecodable, err
		}
		family, ok := strings.CutSuffix(entry.Name(), ".json")
		if entry.IsDir() || !ok || !ValidSID(family) {
			continue
		}
		deletedUndecodable, err := f.sweepGrant(ctx, family, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("sessionstore: sweeping grant %s: %w", family, err))
		}
		if deletedUndecodable {
			undecodable = append(undecodable, family)
		}
	}
	return undecodable, errors.Join(errs...)
}

// sweepGrant deletes family's grant record if it is expired at now or cannot
// be decoded, and reports whether it deleted an undecodable one. Holding
// family's lock stripe keeps a concurrent write from landing between the read
// and the removal.
func (f *FS) sweepGrant(ctx context.Context, family string, now time.Time) (bool, error) {
	lock := f.grantLock(family)
	lock.Lock()
	defer lock.Unlock()
	grant, version, err := f.LoadGrant(ctx, family)
	undecodable := errors.Is(err, errUndecodableGrant)
	switch {
	case undecodable:
	case err != nil:
		return false, err
	case version == 0 || !grant.Expired(now):
		return false, nil
	}
	if err := os.Remove(f.grantPath(family)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("removing grant: %w", err)
	}
	return undecodable, nil
}
