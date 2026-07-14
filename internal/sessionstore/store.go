// Package sessionstore persists per-user Telegram (MTProto) sessions for the
// multi-user HTTP mode. Each authenticated Telegram user gets one opaque
// session blob, keyed by their numeric Telegram user ID.
//
// Backends store ciphertext only: the server wraps any backend with Encrypted
// (AEAD, key derived from the AUTH_TOKEN_KEY master keys), so a leaked bucket
// or directory does not leak live MTProto credentials.
//
// The single-account stdio mode does NOT use this package — it keeps its
// existing Keychain/state-file storage (internal/tgclient).
package sessionstore

import (
	"context"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// Store is a collection of per-user Telegram sessions.
//
// Session returns a gotd session.Storage view bound to one user; its
// LoadSession must return session.ErrNotFound when the user has no session
// yet (that is gotd's "start unauthenticated" signal). Exists is a cheap
// probe used by token refresh to force a re-login after a session was
// deleted. Delete removes the session; the pool builder calls it when
// Telegram refuses a decryptable session (ErrSessionUnauthorized), and an
// operator may call it to force a re-login. A session that fails to decrypt
// is deliberately NOT deleted (see ErrCorruptSession).
type Store interface {
	Session(userID tgid.UserID) session.Storage
	Exists(ctx context.Context, userID tgid.UserID) (bool, error)
	Delete(ctx context.Context, userID tgid.UserID) error
}
