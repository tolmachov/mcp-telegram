package tgclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tgerr"
)

// ErrSessionUnauthorized means the stored session exists but Telegram no
// longer accepts it (never logged in, logged out remotely, or the auth key
// was revoked). The HTTP layer maps this to 401 so the client re-runs the
// OAuth + QR login flow.
var ErrSessionUnauthorized = errors.New("telegram session is not authorized")

// IsSessionUnauthorized identifies both our auth-status verdict and native
// Telegram rejections of the session, including those gotd raises before its
// ready callback. Storage and transport failures must remain distinguishable
// from a dead key.
func IsSessionUnauthorized(err error) bool {
	return errors.Is(err, ErrSessionUnauthorized) || auth.IsUnauthorized(err) ||
		tgerr.Is(err, "AUTH_KEY_UNREGISTERED", "SESSION_REVOKED", "SESSION_EXPIRED", "AUTH_KEY_DUPLICATED", "USER_DEACTIVATED")
}

// IsSystemic reports whether err is a condition of the whole account or call —
// a dead session, a flood wait, or a cancelled/expired context — rather than a
// problem with the one target a request named. Batch callers abort on it
// instead of recording a per-item failure: carrying on would hammer a rate
// limit or a dead session.
func IsSystemic(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if _, ok := tgerr.AsFloodWait(err); ok {
		return true
	}
	return IsSessionUnauthorized(err)
}

// ShouldRefreshPeer identifies stale-access-hash errors for which a caller may
// invalidate and perform exactly one fresh resolve/RPC attempt.
func ShouldRefreshPeer(err error) bool {
	return tgerr.Is(err, "PEER_ID_INVALID", "CHANNEL_INVALID", "CHAT_ID_INVALID")
}

// PeerError is a failure to resolve a chat ID to a peer. Tool layers detect
// it to point the caller at the chat-discovery tools.
type PeerError struct {
	ID  int64
	Err error
}

func (e *PeerError) Error() string { return fmt.Sprintf("resolving chat %d: %v", e.ID, e.Err) }

func (e *PeerError) Unwrap() error { return e.Err }
