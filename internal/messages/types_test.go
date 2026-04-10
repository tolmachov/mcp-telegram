package messages

import (
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGlobalSearchCursorInputPeer verifies that InputPeer reconstructs the
// correct concrete InputPeerClass for each supported peer kind and that the
// access_hash is wired through for user and channel peers.
func TestGlobalSearchCursorInputPeer(t *testing.T) {
	tests := []struct {
		name     string
		cursor   *GlobalSearchCursor
		wantType string
	}{
		{
			name:     "nil cursor returns InputPeerEmpty",
			cursor:   nil,
			wantType: "*tg.InputPeerEmpty",
		},
		{
			name:     "user peer",
			cursor:   &GlobalSearchCursor{PeerKind: PeerKindUser, PeerID: 42, AccessHash: 99, MsgID: 1},
			wantType: "*tg.InputPeerUser",
		},
		{
			name:     "chat peer",
			cursor:   &GlobalSearchCursor{PeerKind: PeerKindChat, PeerID: 1001, MsgID: 1},
			wantType: "*tg.InputPeerChat",
		},
		{
			name:     "channel peer",
			cursor:   &GlobalSearchCursor{PeerKind: PeerKindChannel, PeerID: 9999, AccessHash: -5, MsgID: 1},
			wantType: "*tg.InputPeerChannel",
		},
		{
			name:     "unknown kind returns InputPeerEmpty",
			cursor:   &GlobalSearchCursor{PeerKind: "bot", PeerID: 1, MsgID: 1}, // intentionally invalid kind
			wantType: "*tg.InputPeerEmpty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			peer := tt.cursor.InputPeer()
			got := typeName(peer)
			assert.Equal(t, tt.wantType, got)
		})
	}

	// Verify field values for user and channel — access_hash must be wired through.
	t.Run("user peer fields", func(t *testing.T) {
		c := &GlobalSearchCursor{PeerKind: PeerKindUser, PeerID: 42, AccessHash: 99, MsgID: 1}
		p, ok := c.InputPeer().(*tg.InputPeerUser)
		require.True(t, ok, "expected *tg.InputPeerUser")
		assert.Equal(t, int64(42), p.UserID)
		assert.Equal(t, int64(99), p.AccessHash)
	})

	t.Run("channel peer fields", func(t *testing.T) {
		c := &GlobalSearchCursor{PeerKind: PeerKindChannel, PeerID: 7777, AccessHash: -8, MsgID: 1}
		p, ok := c.InputPeer().(*tg.InputPeerChannel)
		require.True(t, ok, "expected *tg.InputPeerChannel")
		assert.Equal(t, int64(7777), p.ChannelID)
		assert.Equal(t, int64(-8), p.AccessHash)
	})

	t.Run("chat peer field", func(t *testing.T) {
		c := &GlobalSearchCursor{PeerKind: PeerKindChat, PeerID: 1001, MsgID: 1}
		p, ok := c.InputPeer().(*tg.InputPeerChat)
		require.True(t, ok, "expected *tg.InputPeerChat")
		assert.Equal(t, int64(1001), p.ChatID)
	})
}

// typeName returns the Go type name as a string for assertion purposes.
func typeName(v any) string {
	switch v.(type) {
	case *tg.InputPeerEmpty:
		return "*tg.InputPeerEmpty"
	case *tg.InputPeerUser:
		return "*tg.InputPeerUser"
	case *tg.InputPeerChat:
		return "*tg.InputPeerChat"
	case *tg.InputPeerChannel:
		return "*tg.InputPeerChannel"
	default:
		return "unknown"
	}
}
