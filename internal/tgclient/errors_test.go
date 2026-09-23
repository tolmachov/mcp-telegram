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
	assert.True(t, IsSystemic(tgerr.New(401, "SESSION_REVOKED")))
	assert.True(t, IsSystemic(tgerr.New(403, "USER_DEACTIVATED")))
	// A wrapped systemic error stays systemic.
	assert.True(t, IsSystemic(fmt.Errorf("resolving @x: %w", context.Canceled)))
}
