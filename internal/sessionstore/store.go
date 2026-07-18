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

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

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
// when Telegram refuses a decryptable session (ErrSessionUnauthorized), and an
// operator may call it to force a re-login. A session that fails to decrypt is
// deliberately NOT deleted (see ErrCorruptSession).
type Store interface {
	Session(userID tgid.UserID, sid string, userKey []byte) session.Storage
	Exists(ctx context.Context, userID tgid.UserID, sid string) (bool, error)
	Delete(ctx context.Context, userID tgid.UserID, sid string) error
}
