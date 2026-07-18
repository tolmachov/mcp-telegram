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
// when Telegram refuses a decryptable session (ErrSessionUnauthorized), token
// revocation calls it for the revoked session, and an operator may call it to
// force a re-login. A session that fails to decrypt is deliberately NOT
// deleted (see ErrCorruptSession).
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
}

// parseSessionBase reverses the object/file naming shared by the GCS and FS
// backends: "<userID>.bin" (legacy, sid "") or "<userID>.<sid>.bin". ok is
// false for anything else — listings skip such entries instead of failing.
func parseSessionBase(base string) (userID tgid.UserID, sid string, ok bool) {
	name, found := strings.CutSuffix(base, ".bin")
	if !found || name == "" {
		return 0, "", false
	}
	idPart, sidPart, hasSID := strings.Cut(name, ".")
	if hasSID && sidPart == "" {
		return 0, "", false
	}
	id, err := tgid.Parse(idPart)
	if err != nil {
		return 0, "", false
	}
	return id, sidPart, true
}
