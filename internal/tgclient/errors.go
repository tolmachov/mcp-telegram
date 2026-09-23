package tgclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/tgerr"
)

// ErrSessionUnauthorized is the verdict that Telegram no longer accepts the
// stored session (never logged in, logged out remotely, the auth key revoked,
// the account deactivated). Only the home DC delivers it: StartClient's
// readiness check, gotd ending Run on a refused connection, or a refused call
// confirmed on the home DC (see Running.confirmRefusal). The HTTP layer maps
// it to 401 so the client re-runs the OAuth + QR login flow.
var ErrSessionUnauthorized = errors.New("telegram session is not authorized")

// isSessionRefusal reports whether err is one of the replies by which Telegram
// declares a session dead. It is a candidate, not a verdict: a DC other than
// the home one answers the same codes when the authorisation gotd exported to
// it did not take, while the home session is fine. 401s that do not mean
// death when another DC answers a call — SESSION_PASSWORD_NEEDED (a login
// waiting for its 2FA password), AUTH_KEY_PERM_EMPTY (a temporary key not yet
// bound) — are excluded; the home DC's replies are judged by refusedByHome.
func isSessionRefusal(err error) bool {
	return tgerr.Is(err, "AUTH_KEY_UNREGISTERED", "AUTH_KEY_INVALID", "AUTH_KEY_DUPLICATED",
		"SESSION_REVOKED", "SESSION_EXPIRED", "USER_DEACTIVATED", "USER_DEACTIVATED_BAN")
}

// UnconfirmedRefusalError is a call Telegram refused with a session-refusal
// code (see isSessionRefusal) that the home DC did not confirm, so the client
// keeps serving: Check is nil when the home DC still accepts the session, and
// otherwise why it could not be asked.
//
// When the home DC accepts the session, the refusal came from another DC —
// the one a download was sent to — that never took the authorisation gotd
// exported to it. gotd keeps that DC's connection for the rest of the
// client's Run loop and only exports the authorisation to a connection it
// creates, so calls routed there keep being refused until the client
// reconnects; gotd offers no way to drop that connection or export the authorisation to it again.
type UnconfirmedRefusalError struct {
	Refusal error
	Check   error
}

func (e *UnconfirmedRefusalError) Error() string {
	if e.Check == nil {
		return fmt.Sprintf("%v (the account's home Telegram server still accepts the session)", e.Refusal)
	}
	return fmt.Sprintf("%v (the account's home Telegram server could not be asked whether the session still stands: %v)", e.Refusal, e.Check)
}

func (e *UnconfirmedRefusalError) Unwrap() []error {
	if e.Check == nil {
		return []error{e.Refusal}
	}
	return []error{e.Refusal, e.Check}
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
