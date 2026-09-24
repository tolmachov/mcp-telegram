package tgclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tgerr"
)

// ErrSessionUnauthorized is the verdict that Telegram no longer accepts the
// stored session (never logged in, logged out remotely, the auth key revoked,
// the account deactivated). Only the home DC delivers it: StartClient's
// readiness check, gotd ending Run on a refused connection, or the home DC
// refusing a call (see Running.refusalWatch). The HTTP layer maps it to 401
// so the client re-runs the OAuth + QR login flow.
var ErrSessionUnauthorized = errors.New("telegram session is not authorized")

// ErrSecondaryRefusal is a session refusal (isSessionRefusal) answered by a
// Telegram DC other than the account's home one: the DC gotd sent a file
// download or a statistics call to refused the authorisation gotd exported
// to it. It says nothing about the session itself, which only the home DC
// judges (see Running.refusalWatch).
var ErrSecondaryRefusal = errors.New("a Telegram DC other than the account's home one refused the session")

// isSessionRefusal reports whether err is one of the replies by which Telegram
// declares a session unusable: a 401, or one of the other codes Telegram ends
// a session with. From the home DC it is the verdict; from any other DC it
// says only that the DC refused the authorisation gotd exported to it.
func isSessionRefusal(err error) bool {
	return auth.IsUnauthorized(err) || tgerr.Is(err, "AUTH_KEY_UNREGISTERED", "AUTH_KEY_INVALID", "AUTH_KEY_DUPLICATED",
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

// ErrUnresolvablePeer is the failure to turn a chat reference into a peer
// because of the reference itself: a non-positive ID, an ID that is no
// reachable user, chat or channel, a channel this account cannot access, or an
// entity without the access hash an InputPeer needs. Any other resolve failure
// — a flood wait, a transport error, Telegram's own trouble — says nothing
// about the chat and is not this error.
var ErrUnresolvablePeer = errors.New("cannot resolve chat")

// unresolvable reports that chat id cannot be resolved, and why.
func unresolvable(id int64, reason string) error {
	return fmt.Errorf("%w %d: %s", ErrUnresolvablePeer, id, reason)
}

// IsPeerSpecific reports whether err, which is not systemic (callers test
// IsSystemic first), is about one peer a request named — a chat reference that
// cannot be resolved (ErrUnresolvablePeer), a stale or invalid ID or access
// hash, a channel this account cannot access, an unknown @username — so a
// batch Telegram failed as a whole may retry its peers one by one to isolate
// the bad one. Any other error would fail each of them alike.
func IsPeerSpecific(err error) bool {
	return errors.Is(err, ErrUnresolvablePeer) || ShouldRefreshPeer(err) ||
		tgerr.Is(err, "CHANNEL_PRIVATE", "CHANNEL_PUBLIC_GROUP_NA", "PEER_ID_NOT_SUPPORTED", "USER_BANNED_IN_CHANNEL",
			"USERNAME_NOT_OCCUPIED", "USERNAME_INVALID")
}
