// Package sessionstore persists per-user Telegram (MTProto) sessions for the
// multi-user HTTP mode. Each authorization gets its own opaque session blob,
// keyed by the numeric Telegram user ID plus a random per-authorization
// session id (sid), so one account may hold several independent sessions at
// once (one per logged-in client) instead of fighting over a single object.
//
// Backends store ciphertext only: the server wraps any backend with Encrypted
// (AEAD). The v2 key is derived from BOTH the AUTH_TOKEN_KEY master keys AND a
// random per-session key that lives only inside the client's OAuth token
// (never persisted here), so a leaked bucket + secret manager, without a live
// token, cannot decrypt a session. Legacy blobs (empty sid, empty userKey)
// stay decryptable with the master keys alone for backward compatibility.
//
// The single-account stdio mode does NOT use this package — it keeps its
// existing Keychain/state-file storage (internal/tgclient).
package sessionstore

import (
	"context"
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

// sessionBase builds the object/file base name for a session: "<userID>.bin"
// (legacy, sid "") or "<userID>.<sid>.bin". It is the single source of truth
// for the naming scheme; parseSessionBase is its inverse.
func sessionBase(userID tgid.UserID, sid string) string {
	if sid == "" {
		return userID.String() + ".bin"
	}
	return userID.String() + "." + sid + ".bin"
}

// SessionRef identifies one stored session and when its blob was last
// written. UpdatedAt is the storage-layer modification time (object mtime),
// which is always >= the LoginAt of any token bound to the session — login,
// upgrade-on-refresh, and every gotd re-store all write the blob. The orphan
// sweeper relies on that invariant: a blob older than the refresh-token TTL
// cannot be reached by any live grant.
type SessionRef struct {
	UserID tgid.UserID
	// SID is the per-authorization session id; "" for the legacy per-user
	// object.
	SID       string
	UpdatedAt time.Time
}

// Store is a collection of per-authorization Telegram sessions.
//
// Session returns a gotd session.Storage view bound to one (userID, sid)
// session; its LoadSession must return session.ErrNotFound when that session
// does not exist yet (gotd's "start unauthenticated" signal). userKey is the
// per-session secret from the OAuth token that the Encrypted wrapper mixes
// into the AEAD key; the storage backends themselves ignore it (they only ever
// hold ciphertext). An empty sid selects the legacy per-user object and an
// empty userKey selects the legacy master-only encryption, for pre-upgrade
// sessions.
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
}

// parseSessionBase reverses sessionBase: "<userID>.bin" (legacy, sid "") or
// "<userID>.<sid>.bin". ok is false for anything else — a legacy name with a
// non-numeric id, or a suffixed name whose sid is not a valid session id (an
// operator's "123.bak.bin", a stray file). Listings skip such entries, so the
// sweeper only ever deletes objects this package itself could have written.
func parseSessionBase(base string) (userID tgid.UserID, sid string, ok bool) {
	name, found := strings.CutSuffix(base, ".bin")
	if !found || name == "" {
		return 0, "", false
	}
	idPart, sidPart, hasSID := strings.Cut(name, ".")
	if hasSID && !ValidSID(sidPart) {
		return 0, "", false
	}
	id, err := tgid.Parse(idPart)
	if err != nil {
		return 0, "", false
	}
	return id, sidPart, true
}
