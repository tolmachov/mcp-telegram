package tgclient

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gotd/td/tgerr"
	"github.com/stretchr/testify/assert"
)

// TestIsSystemic distinguishes account- or call-wide failures from problems
// with one request's target.
func TestIsSystemic(t *testing.T) {
	assert.False(t, IsSystemic(nil))
	assert.False(t, IsSystemic(errors.New("plain failure")))
	assert.False(t, IsSystemic(tgerr.New(400, "USERNAME_NOT_OCCUPIED")))
	assert.True(t, IsSystemic(context.Canceled))
	assert.True(t, IsSystemic(context.DeadlineExceeded))
	assert.True(t, IsSystemic(&tgerr.Error{Code: 420, Type: "FLOOD_WAIT", Message: "FLOOD_WAIT_5", Argument: 5}))
	// A dead session is systemic once confirmed; Telegram's bare reply to one
	// call is not (see Running.confirmRefusal).
	assert.True(t, IsSystemic(fmt.Errorf("%w: %w", ErrSessionUnauthorized, tgerr.New(401, "SESSION_REVOKED"))))
	assert.False(t, IsSystemic(tgerr.New(401, "AUTH_KEY_UNREGISTERED")))
	// A wrapped systemic error stays systemic.
	assert.True(t, IsSystemic(fmt.Errorf("resolving @x: %w", context.Canceled)))
}

// TestIsPeerSpecific separates failures about one named peer, which a batch may
// isolate by retrying its peers one by one, from failures that would fail
// every peer alike.
func TestIsPeerSpecific(t *testing.T) {
	for _, err := range []error{
		tgerr.New(400, "CHANNEL_INVALID"),
		tgerr.New(400, "PEER_ID_INVALID"),
		tgerr.New(406, "CHANNEL_PRIVATE"),
		fmt.Errorf("getting top messages: %w", tgerr.New(400, "CHANNEL_PRIVATE")),
		&PeerError{ID: 5, Err: errors.New("no such chat")},
	} {
		assert.True(t, IsPeerSpecific(err), "%v is about one peer", err)
	}
	for _, err := range []error{
		errors.New("connection reset by peer"),
		tgerr.New(500, "INTERNAL_SERVER_ERROR"),
		tgerr.New(400, "LIMIT_INVALID"),
	} {
		assert.False(t, IsPeerSpecific(err), "%v would fail every peer alike", err)
	}
}
