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

// ErrClientStopped is the failure of a call that could not complete because
// the Telegram client it went through stopped: the error wraps why the client
// stopped (Running.Err). It replaces whatever gotd's shutdown left the call
// with — a cancelled context, a closed connection — which says nothing of the
// cause and would pass for the caller's own cancellation.
var ErrClientStopped = errors.New("the Telegram client stopped")

// Stopped is the failure of a call the client could not serve because it
// stopped for reason: ErrClientStopped wrapping reason.
func Stopped(reason error) error { return fmt.Errorf("%w: %w", ErrClientStopped, reason) }

// isSessionRefusal reports whether err is one of the replies by which Telegram
// declares a session unusable: a 401, or one of the other codes Telegram ends
// a session with. From the home DC it is the verdict; from any other DC it
// says only that the DC refused the authorisation gotd exported to it.
func isSessionRefusal(err error) bool {
	return auth.IsUnauthorized(err) || tgerr.Is(err, "AUTH_KEY_UNREGISTERED", "AUTH_KEY_INVALID", "AUTH_KEY_DUPLICATED",
		"SESSION_REVOKED", "SESSION_EXPIRED", "USER_DEACTIVATED", "USER_DEACTIVATED_BAN")
}

// IsSystemic reports whether err ends a batch: a condition of the whole
// account or call rather than of the one target a request named, so carrying
// on would hammer a rate limit, a dead session or a stopped client, or run
// past the call's end. It is every condition IsBeyondRequest names but one
// chat's slow mode, which says nothing about the batch's other chats, plus the
// call running out of time. Batch callers abort on it instead of recording a
// per-item failure.
func IsSystemic(err error) bool {
	if wait, ok := RetryAfter(err); ok {
		return wait.Scope != ScopeChat
	}
	return errors.Is(err, context.DeadlineExceeded) || IsBeyondRequest(err)
}

// IsBeyondRequest reports whether err is a condition no change to the request
// can cure — a wait Telegram told the call to take (RetryAfter), a dead
// session (ErrSessionUnauthorized), a stopped client (ErrClientStopped) or the
// caller cancelling — so a hint on how to change the request is wrong for it.
// A call that ran out of time is not one: a smaller request may finish in
// time.
func IsBeyondRequest(err error) bool {
	if _, ok := RetryAfter(err); ok {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, ErrClientStopped) || errors.Is(err, ErrSessionUnauthorized)
}

// shouldRefreshPeer identifies stale-access-hash errors for which WithPeer
// drops the cached peer and runs its call once more on a fresh resolve.
func shouldRefreshPeer(err error) bool {
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
	return errors.Is(err, ErrUnresolvablePeer) || shouldRefreshPeer(err) ||
		tgerr.Is(err, "CHANNEL_PRIVATE", "CHANNEL_PUBLIC_GROUP_NA", "PEER_ID_NOT_SUPPORTED", "USER_BANNED_IN_CHANNEL",
			"USERNAME_NOT_OCCUPIED", "USERNAME_INVALID")
}
