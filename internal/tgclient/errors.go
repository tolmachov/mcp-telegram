package tgclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/tgerr"
)

// ErrSessionUnauthorized is the verdict that Telegram no longer accepts the
// stored session (never logged in, logged out remotely, the auth key revoked,
// the account deactivated). Only the home DC delivers it: StartClient's auth
// check, gotd ending Run on a refused connection, or a refused call confirmed
// on the home DC (see Running.confirmRefusal). The HTTP layer maps it to 401
// so the client re-runs the OAuth + QR login flow.
var ErrSessionUnauthorized = errors.New("telegram session is not authorized")

// isSessionRefusal reports whether err is one of the replies by which Telegram
// declares a session dead. It is a candidate, not a verdict: a DC other than
// the home one answers the same codes when the authorisation gotd exported to
// it did not take, while the home session is fine. 401s that do not mean
// death — SESSION_PASSWORD_NEEDED (a login waiting for its 2FA password),
// AUTH_KEY_PERM_EMPTY (a temporary key not yet bound) — are excluded.
func isSessionRefusal(err error) bool {
	return tgerr.Is(err, "AUTH_KEY_UNREGISTERED", "AUTH_KEY_INVALID", "AUTH_KEY_DUPLICATED",
		"SESSION_REVOKED", "SESSION_EXPIRED", "USER_DEACTIVATED", "USER_DEACTIVATED_BAN")
}

// IsSystemic reports whether err is a condition of the whole account or call —
// a dead session (ErrSessionUnauthorized), a flood wait, or a
// cancelled/expired context — rather than a problem with the one target a
// request named. Batch callers abort on it instead of recording a per-item
// failure: carrying on would hammer a rate limit or a dead session.
func IsSystemic(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if _, ok := tgerr.AsFloodWait(err); ok {
		return true
	}
	return errors.Is(err, ErrSessionUnauthorized)
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
