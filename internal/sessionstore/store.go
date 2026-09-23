// Package sessionstore persists per-authorization Telegram (MTProto) sessions
// for the multi-user HTTP mode. Each authorization gets its own opaque session blob,
// keyed by the numeric Telegram user ID plus a random per-authorization
// session id (sid), so one account may hold several independent sessions at
// once (one per logged-in client) instead of fighting over a single object.
//
// Encrypted is the only way to obtain a Store: it validates every session id
// and grant family before a backend sees it, and backends (FS, GCS) hold AEAD
// ciphertext only. The v3 key is derived from BOTH the MCP_AUTH_TOKEN_KEYS
// master keys AND a random per-session key that lives only inside the client's
// OAuth token (never persisted here), so a leaked bucket + secret manager,
// without a live token, cannot decrypt a session. Earlier formats and empty session ids are
// intentionally unreadable.
//
// The single-account stdio mode does NOT use this package — it keeps its
// existing Keychain/state-file storage (internal/tgclient).
package sessionstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// sidBytes is the number of random bytes in a session id: 128 bits is
// collision-safe across a deployment's sessions.
const sidBytes = 16

// sidHexLen is the character length of a session id, the hex of sidBytes.
const sidHexLen = 2 * sidBytes

// sessionKeyLen is the byte length of a per-session key, the AES-256 key size.
const sessionKeyLen = 32

// NewSID returns a fresh random session id. Grant families use the same
// format, so it mints those too.
func NewSID() string {
	b := make([]byte, sidBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}

// NewSessionKey returns a fresh random per-session key, the share of the
// session encryption key that lives only in the client's OAuth token. It is a
// secret: never log it.
func NewSessionKey() []byte {
	k := make([]byte, sessionKeyLen)
	_, _ = rand.Read(k) // crypto/rand.Read never returns an error
	return k
}

// ValidSessionKey reports whether k is a well-formed per-session key — the
// length NewSessionKey mints. A shorter key would weaken the split-key
// protection, so the Encrypted store refuses any other.
func ValidSessionKey(k []byte) bool { return len(k) == sessionKeyLen }

// ValidSID reports whether s is a well-formed session id — exactly sidHexLen
// lowercase hex characters. Session ids reach this layer as an object-name /
// file-path suffix, so the Encrypted store rejects any other value before it
// can influence a path; parseSessionBase applies the same rule so the sweeper
// never attributes (and thus never deletes) a foreign, operator-named object.
func ValidSID(s string) bool {
	if len(s) != sidHexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// sessionBase builds the object/file base name for a session.
func sessionBase(userID tgid.UserID, sid string) string {
	return userID.String() + "." + sid + ".bin"
}

// SessionRef identifies one stored session and when its blob was last
// written. UpdatedAt is the storage-layer modification time (object mtime).
// Login and every gotd re-store both write the blob, so
// mtime tracks the newest grant bound to the session — except at login, where
// the blob is written BEFORE the authorization code is redeemed, so mtime may
// lag that grant's LoginAt by up to codeTTL (60s). The orphan sweeper relies
// on this: a blob older than refresh-token TTL + sweepMargin cannot be reached
// by any live grant (the 24h margin dwarfs the 60s login lag).
type SessionRef struct {
	UserID tgid.UserID
	// SID is the per-authorization session id.
	SID       string
	UpdatedAt time.Time
}

// GrantRotation is the outcome of RotateGrant. The zero value is a refusal,
// so an outcome that was never set cannot read as a successful rotation.
type GrantRotation int

const (
	// GrantMissing means the family does not exist or has expired.
	GrantMissing GrantRotation = iota
	// GrantReplay means a stale generation (or a revoked family) was presented;
	// the whole family is now revoked.
	GrantReplay
	// GrantRotated means the presented generation was current and is now
	// superseded by the next one.
	GrantRotated
)

// Store is a collection of per-authorization Telegram sessions.
//
// Session returns a gotd session.Storage view bound to one (userID, sid)
// session; its LoadSession must return session.ErrNotFound when that session
// does not exist yet (gotd's "start unauthenticated" signal). userKey is the
// per-session secret from the OAuth token that is mixed into the AEAD key.
// Both sid and userKey are mandatory and must be well formed (ValidSID,
// ValidSessionKey).
//
// Exists is a cheap probe used by token refresh to force a re-login after a
// session was deleted. Delete removes one session; the user pool deletes a
// decryptable session Telegram refused (ErrSessionUnauthorized) — whether a
// client was starting on it or already running — and an operator may call it
// to force a re-login. A session that fails to decrypt is
// deliberately NOT deleted (see ErrCorruptSession).
//
// Revocation is durable and independent of the blob: Revoke writes a tombstone
// (and deletes the blob) and Revoked reports it. Because a live gotd client can
// re-store (resurrect) a deleted blob, deletion alone cannot revoke; the
// tombstone lives where the client never writes, so it survives a re-store and
// the refresh grant checks Revoked. ListRevoked enumerates tombstones for the
// sweeper to reclaim once no live refresh token could reference them.
//
// List enumerates every stored session; the orphan sweeper uses it to reclaim
// sessions whose blobs are older than any live refresh grant could be.
// Backends skip entries they cannot attribute (malformed names) rather than
// failing the whole listing.
type Store interface {
	Session(userID tgid.UserID, sid string, userKey []byte) session.Storage
	Exists(ctx context.Context, userID tgid.UserID, sid string) (bool, error)
	Delete(ctx context.Context, userID tgid.UserID, sid string) error
	List(ctx context.Context) ([]SessionRef, error)

	// Revoke durably marks (userID, sid) revoked (a tombstone) and deletes its
	// session blob. Idempotent.
	Revoke(ctx context.Context, userID tgid.UserID, sid string) error
	// Revoked reports whether (userID, sid) has a revocation tombstone.
	Revoked(ctx context.Context, userID tgid.UserID, sid string) (bool, error)
	// ListRevoked enumerates revocation tombstones (with their write time) so
	// the sweeper can reclaim ones older than any live refresh token.
	ListRevoked(ctx context.Context) ([]SessionRef, error)
	// DeleteRevoked removes a revocation tombstone; the sweeper calls it once no
	// live refresh token could reference the session.
	DeleteRevoked(ctx context.Context, userID tgid.UserID, sid string) error

	// RedeemCode atomically creates generation zero of family's refresh grant,
	// expiring at expiresAt. The family is the authorization code's random id,
	// so false means the code was already redeemed.
	RedeemCode(ctx context.Context, family string, expiresAt time.Time) (bool, error)
	// RotateGrant performs generation expected -> expected+1 at now.
	// Presenting any other generation (or a revoked family) revokes the family
	// and returns GrantReplay.
	RotateGrant(ctx context.Context, family string, expected int64, now time.Time) (GrantRotation, error)
	// RevokeGrant marks family revoked so none of its refresh tokens rotate
	// again. A missing family is already dead.
	RevokeGrant(ctx context.Context, family string) error
	// SweepAuthState deletes grant records that are expired at now.
	SweepAuthState(ctx context.Context, now time.Time) error

	// encrypted restricts implementations to the store Encrypted returns (and
	// wrappers embedding it), so no Store can skip identity validation.
	encrypted()
}

// backend is a storage backend (FS, GCS, the test memory store): it keeps
// ciphertext blobs under ids Encrypted has already validated. The session
// methods mean what they mean on Store; Session takes no key because a backend
// never decrypts. For grants a backend offers only the compare-and-swap the
// Store's grant rules are built on: LoadGrant returns family's record and an
// opaque non-zero version (0: no record), and StoreGrant writes a record only
// if the stored version still equals version (0: only if none exists),
// returning ErrGrantConflict otherwise.
type backend interface {
	Session(userID tgid.UserID, sid string) session.Storage
	Exists(ctx context.Context, userID tgid.UserID, sid string) (bool, error)
	Delete(ctx context.Context, userID tgid.UserID, sid string) error
	List(ctx context.Context) ([]SessionRef, error)
	Revoke(ctx context.Context, userID tgid.UserID, sid string) error
	Revoked(ctx context.Context, userID tgid.UserID, sid string) (bool, error)
	ListRevoked(ctx context.Context) ([]SessionRef, error)
	DeleteRevoked(ctx context.Context, userID tgid.UserID, sid string) error
	LoadGrant(ctx context.Context, family string) (grant GrantRecord, version int64, err error)
	StoreGrant(ctx context.Context, family string, grant GrantRecord, version int64) error
	SweepAuthState(ctx context.Context, now time.Time) error
}

// parseSessionBase reverses sessionBase. Listings skip anything that does not
// use the current canonical format.
func parseSessionBase(base string) (userID tgid.UserID, sid string, ok bool) {
	name, found := strings.CutSuffix(base, ".bin")
	if !found || name == "" {
		return 0, "", false
	}
	idPart, sidPart, hasSID := strings.Cut(name, ".")
	if !hasSID || !ValidSID(sidPart) {
		return 0, "", false
	}
	id, err := tgid.Parse(idPart)
	if err != nil {
		return 0, "", false
	}
	// Round-trip check: tgid.Parse accepts non-canonical spellings ("07", "+7")
	// that sessionBase never emits. Without this, a foreign file like 07.bin
	// would alias to user 7 and the sweeper would delete the CANONICAL 7.bin (or
	// its tombstone) instead of skipping the foreign object — breaking the "only
	// delete what this package could have written" invariant.
	if idPart != id.String() {
		return 0, "", false
	}
	return id, sidPart, true
}

// GrantRecord is the persisted refresh-grant state of one authorization-code
// family.
type GrantRecord struct {
	Generation int64     `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
	Revoked    bool      `json:"revoked,omitempty"`
	// WriteID is a random id every grant write stamps on the record it
	// stores, so a write whose response was lost recognises its own record on
	// re-read instead of taking it for a concurrent writer's.
	WriteID string `json:"write_id,omitempty"`
}

// Expired reports whether the grant is past its expiry at now. A zero
// ExpiresAt counts as expired: RedeemCode always sets one, so a record without
// it is malformed and must not keep a family alive.
func (g GrantRecord) Expired(now time.Time) bool { return !now.Before(g.ExpiresAt) }

// rotate applies one refresh with the presented generation: the current
// generation advances, anything else (or a revoked family) revokes the family.
func (g GrantRecord) rotate(expected int64) (GrantRecord, GrantRotation) {
	if g.Revoked || g.Generation != expected {
		g.Revoked = true
		return g, GrantReplay
	}
	g.Generation++
	return g, GrantRotated
}

// ErrGrantConflict is returned by a backend's StoreGrant when the record
// changed since it was loaded (or already exists, for a create).
var ErrGrantConflict = errors.New("sessionstore: grant changed concurrently")

// grantCASAttempts bounds the load/compare-and-swap retries of one grant update.
const grantCASAttempts = 4

func (s *encryptedStore) RedeemCode(ctx context.Context, family string, expiresAt time.Time) (bool, error) {
	if !ValidSID(family) {
		return false, ErrInvalidSID
	}
	created, err := writeGrant(ctx, s.inner, family, GrantRecord{ExpiresAt: expiresAt}, 0)
	if err != nil {
		return false, fmt.Errorf("creating grant: %w", err)
	}
	return created, nil
}

func (s *encryptedStore) RotateGrant(ctx context.Context, family string, expected int64, now time.Time) (GrantRotation, error) {
	if !ValidSID(family) {
		return GrantMissing, ErrInvalidSID
	}
	var result GrantRotation
	err := updateGrant(ctx, s.inner, family, func(g GrantRecord) (GrantRecord, bool) {
		if g.Expired(now) {
			result = GrantMissing
			return g, false
		}
		g, result = g.rotate(expected)
		return g, true
	})
	if errors.Is(err, errGrantAbsent) {
		return GrantMissing, nil
	}
	if err != nil {
		return GrantMissing, fmt.Errorf("rotating grant: %w", err)
	}
	return result, nil
}

func (s *encryptedStore) RevokeGrant(ctx context.Context, family string) error {
	if !ValidSID(family) {
		return ErrInvalidSID
	}
	err := updateGrant(ctx, s.inner, family, func(g GrantRecord) (GrantRecord, bool) {
		g.Revoked = true
		return g, true
	})
	if errors.Is(err, errGrantAbsent) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("revoking grant: %w", err)
	}
	return nil
}

var errGrantAbsent = errors.New("sessionstore: grant not found")

// updateGrant is the one compare-and-swap loop over a grant record: it loads
// the record, applies change, and stores the result unless another writer got
// there first, in which case it retries on the fresh record. change reports
// whether anything should be written; its last call is the one whose record
// was stored.
func updateGrant(ctx context.Context, b backend, family string, change func(GrantRecord) (GrantRecord, bool)) error {
	for range grantCASAttempts {
		grant, version, err := b.LoadGrant(ctx, family)
		if err != nil {
			return fmt.Errorf("loading grant: %w", err)
		}
		if version == 0 {
			return errGrantAbsent
		}
		next, write := change(grant)
		if !write {
			return nil
		}
		stored, err := writeGrant(ctx, b, family, next, version)
		if err != nil {
			return err
		}
		if stored {
			return nil
		}
	}
	return fmt.Errorf("%w: gave up after %d attempts", ErrGrantConflict, grantCASAttempts)
}

// writeGrant stores grant at version under a fresh WriteID and reports
// whether it is now the stored record; false means another writer got there
// first. A failed write is settled by re-reading: a write can land and still
// report failure (a GCS retry of a request whose response was lost answers
// 412, a transport error can follow a committed upload), and finding its own
// WriteID in the store is what tells that apart from a lost race. When the
// re-read fails too, the outcome is unknown and the error carries both.
func writeGrant(ctx context.Context, b backend, family string, grant GrantRecord, version int64) (bool, error) {
	grant.WriteID = rand.Text()
	err := b.StoreGrant(ctx, family, grant, version)
	if err == nil {
		return true, nil
	}
	stored, _, loadErr := b.LoadGrant(ctx, family)
	if loadErr != nil {
		return false, fmt.Errorf("storing grant: outcome unknown, as re-reading it failed too: %w", errors.Join(err, loadErr))
	}
	if stored.WriteID == grant.WriteID {
		return true, nil
	}
	if errors.Is(err, ErrGrantConflict) {
		return false, nil
	}
	return false, fmt.Errorf("storing grant: %w", err)
}
