// Package sessionstore persists per-authorization Telegram (MTProto) sessions
// for the multi-user HTTP mode. Each authorization gets its own opaque session blob,
// keyed by the numeric Telegram user ID plus a random per-authorization
// session id (sid), so one account may hold several independent sessions at
// once (one per logged-in client) instead of fighting over a single object.
//
// Backends store ciphertext only: the server wraps any backend with Encrypted
// (AEAD). The v3 key is derived from BOTH the MCP_AUTH_TOKEN_KEYS master keys AND a
// random per-session key that lives only inside the client's OAuth token
// (never persisted here), so a leaked bucket + secret manager, without a live
// token, cannot decrypt a session. Earlier formats and empty session ids are
// intentionally unreadable.
//
// The single-account stdio mode does NOT use this package — it keeps its
// existing Keychain/state-file storage (internal/tgclient).
package sessionstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// sidHexLen is the character length of a session id: hex of 16 random bytes.
const sidHexLen = 32

// ValidSID reports whether s is a well-formed session id — exactly sidHexLen
// lowercase hex characters. Session ids reach this layer as an object-name /
// file-path suffix, so callers that take a sid from an untrusted source (a
// token blob) must validate it with ValidSID before it can influence a path;
// parseSessionBase applies the same rule so the sweeper never attributes (and
// thus never deletes) a foreign, operator-named object.
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

// GrantRotation is the outcome of RotateGrant.
type GrantRotation int

const (
	// GrantRotated means the presented generation was current and is now
	// superseded by the next one.
	GrantRotated GrantRotation = iota
	// GrantReplay means a stale generation (or a revoked family) was presented;
	// the whole family is now revoked.
	GrantReplay
	// GrantMissing means the family does not exist or has expired.
	GrantMissing
)

// Store is a collection of per-authorization Telegram sessions.
//
// Session returns a gotd session.Storage view bound to one (userID, sid)
// session; its LoadSession must return session.ErrNotFound when that session
// does not exist yet (gotd's "start unauthenticated" signal). userKey is the
// per-session secret from the OAuth token that the Encrypted wrapper mixes
// into the AEAD key; the storage backends themselves ignore it (they only ever
// hold ciphertext). Both sid and userKey are mandatory.
//
// Exists is a cheap probe used by token refresh to force a re-login after a
// session was deleted. Delete removes one session; the pool builder calls it
// when Telegram refuses a decryptable session (ErrSessionUnauthorized) and an
// operator may call it to force a re-login. A session that fails to decrypt is
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

	// LoadGrant returns family's refresh-grant record and an opaque non-zero
	// version for StoreGrant; version 0 means no record exists.
	LoadGrant(ctx context.Context, family string) (grant GrantRecord, version int64, err error)
	// StoreGrant writes family's record only if the stored version still equals
	// version (0: only if none exists), and returns ErrGrantConflict otherwise.
	// The grant policy lives in RedeemCode, RotateGrant and RevokeGrant.
	StoreGrant(ctx context.Context, family string, grant GrantRecord, version int64) error
	// SweepAuthState deletes grant records that are expired at now.
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
	SID        string    `json:"sid"`
	Generation int64     `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
	Revoked    bool      `json:"revoked,omitempty"`
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

// ErrGrantConflict is returned by StoreGrant when the record changed since it
// was loaded (or already exists, for a create).
var ErrGrantConflict = errors.New("sessionstore: grant changed concurrently")

// grantCASAttempts bounds the load/compare-and-swap retries of one grant update.
const grantCASAttempts = 4

// RedeemCode atomically creates generation zero for a new OAuth grant. The
// family is the authorization code's random jti, so an existing record means
// the code was already redeemed and false is returned.
func RedeemCode(ctx context.Context, s Store, family, sid string, expiresAt time.Time) (bool, error) {
	err := s.StoreGrant(ctx, family, GrantRecord{SID: sid, ExpiresAt: expiresAt}, 0)
	if errors.Is(err, ErrGrantConflict) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("creating grant: %w", err)
	}
	return true, nil
}

// RotateGrant performs generation N -> N+1 at now. Presenting any stale
// generation revokes the family and returns GrantReplay.
func RotateGrant(ctx context.Context, s Store, family string, expected int64, now time.Time) (GrantRotation, error) {
	var result GrantRotation
	err := updateGrant(ctx, s, family, func(g GrantRecord) (GrantRecord, bool) {
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
		return GrantMissing, err
	}
	return result, nil
}

// RevokeGrant marks family revoked so none of its refresh tokens rotate again.
// A missing family is already dead.
func RevokeGrant(ctx context.Context, s Store, family string) error {
	err := updateGrant(ctx, s, family, func(g GrantRecord) (GrantRecord, bool) {
		g.Revoked = true
		return g, true
	})
	if errors.Is(err, errGrantAbsent) {
		return nil
	}
	return err
}

var errGrantAbsent = errors.New("sessionstore: grant not found")

// updateGrant is the one compare-and-swap loop over a grant record: it loads
// the record, applies change, and stores the result unless another writer got
// there first, in which case it retries on the fresh record. change reports
// whether anything should be written.
func updateGrant(ctx context.Context, s Store, family string, change func(GrantRecord) (GrantRecord, bool)) error {
	for range grantCASAttempts {
		grant, version, err := s.LoadGrant(ctx, family)
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
		err = s.StoreGrant(ctx, family, next, version)
		if errors.Is(err, ErrGrantConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("storing grant: %w", err)
		}
		return nil
	}
	return fmt.Errorf("%w: gave up after %d attempts", ErrGrantConflict, grantCASAttempts)
}
