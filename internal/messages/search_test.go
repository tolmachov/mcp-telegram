package messages

import (
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestMessage builds a minimal *tg.Message suitable for processGlobalHistory.
func newTestMessage(id int, peer tg.PeerClass, fromID tg.PeerClass, text string) *tg.Message {
	m := &tg.Message{
		ID:      id,
		PeerID:  peer,
		Date:    1_700_000_000 + id,
		Message: text,
	}
	if fromID != nil {
		m.SetFromID(fromID)
	}
	return m
}

// TestProcessGlobalHistoryPeerKinds covers the three valid peer kinds and
// asserts the ChatID is the bare MTProto id (positive for every peer type).
func TestProcessGlobalHistoryPeerKinds(t *testing.T) {
	p := &Provider{}

	users := []tg.UserClass{
		&tg.User{ID: 42, FirstName: "Alice", AccessHash: 111},
	}
	chats := []tg.ChatClass{
		&tg.Chat{ID: 500, Title: "LegacyChat"},
		&tg.Channel{ID: 600, Title: "BroadcastChan", AccessHash: 222},
	}

	tests := []struct {
		name         string
		peer         tg.PeerClass
		fromID       tg.PeerClass
		wantChatID   int64
		wantChatName string
		wantSender   string
	}{
		{
			name:         "PeerUser DM (nil FromID → sender is the peer)",
			peer:         &tg.PeerUser{UserID: 42},
			fromID:       nil,
			wantChatID:   42,
			wantChatName: "Alice",
			wantSender:   "Alice",
		},
		{
			name:         "PeerChat basic chat",
			peer:         &tg.PeerChat{ChatID: 500},
			fromID:       &tg.PeerUser{UserID: 42},
			wantChatID:   500,
			wantChatName: "LegacyChat",
			wantSender:   "Alice",
		},
		{
			name:         "PeerChannel broadcast",
			peer:         &tg.PeerChannel{ChannelID: 600},
			fromID:       &tg.PeerUser{UserID: 42},
			wantChatID:   600,
			wantChatName: "BroadcastChan",
			wantSender:   "Alice",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			history := &tg.MessagesMessages{
				Messages: []tg.MessageClass{newTestMessage(1, tt.peer, tt.fromID, "hi")},
				Users:    users,
				Chats:    chats,
			}
			res, err := p.processGlobalHistory(history, 50)
			require.NoError(t, err)
			require.Len(t, res.Messages, 1)
			got := res.Messages[0]
			assert.Equal(t, tt.wantChatID, got.ChatID)
			assert.Equal(t, tt.wantChatName, got.ChatTitle)
			assert.Equal(t, tt.wantSender, got.SenderName)
		})
	}
}

// TestProcessGlobalHistoryCursorSkipsServiceMessages is the regression guard
// for the C1 bug: a full raw page containing MessageService/MessageEmpty
// slots must still emit a next cursor, because the raw page length signals
// "more pages available" — the fact that we filtered some slots out is a
// client-side choice and shouldn't truncate pagination.
func TestProcessGlobalHistoryCursorSkipsServiceMessages(t *testing.T) {
	p := &Provider{}
	peer := &tg.PeerUser{UserID: 42}
	users := []tg.UserClass{&tg.User{ID: 42, FirstName: "Alice", AccessHash: 111}}

	// Build a raw page of limit=3 with one service message at the tail.
	// After filtering, result.Messages has only 2 entries — but the cursor
	// must still be emitted because the raw page is full, and it must anchor
	// to the last RAW item (ID=3), not the last rendered one (ID=2), or the
	// next page will duplicate the tail.
	raw := []tg.MessageClass{
		newTestMessage(1, peer, &tg.PeerUser{UserID: 42}, "one"),
		newTestMessage(2, peer, &tg.PeerUser{UserID: 42}, "two"),
		&tg.MessageService{ID: 3, PeerID: peer}, // filtered out but still advances pagination
	}
	history := &tg.MessagesMessagesSlice{
		Count:    100,
		Messages: raw,
		Users:    users,
	}

	res, err := p.processGlobalHistory(history, 3)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Count)
	assert.Equal(t, 1, res.SkippedCount)
	assert.True(t, res.HasMore, "HasMore = false, want true — raw page is full so pagination should continue")
	require.NotNil(t, res.NextCursor)
	assert.Equal(t, 3, res.NextCursor.MsgID)
	assert.Equal(t, PeerKindUser, res.NextCursor.PeerKind)
	assert.Equal(t, int64(111), res.NextCursor.AccessHash)
}

// TestProcessGlobalHistoryCursorAllServiceMessages ensures pagination does not
// terminate just because a full raw page contains only service messages. They
// are filtered out of the rendered result but still provide a valid cursor
// anchor for continuing the search.
func TestProcessGlobalHistoryCursorAllServiceMessages(t *testing.T) {
	p := &Provider{}
	peer := &tg.PeerUser{UserID: 42}
	users := []tg.UserClass{&tg.User{ID: 42, FirstName: "Alice", AccessHash: 111}}

	raw := []tg.MessageClass{
		&tg.MessageService{ID: 1, PeerID: peer},
		&tg.MessageService{ID: 2, PeerID: peer},
		&tg.MessageService{ID: 3, PeerID: peer},
	}
	history := &tg.MessagesMessagesSlice{
		Count:    100,
		Messages: raw,
		Users:    users,
	}

	res, err := p.processGlobalHistory(history, 3)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Count)
	assert.Equal(t, 3, res.SkippedCount)
	assert.True(t, res.HasMore, "HasMore = false, want true for a full raw page")
	require.NotNil(t, res.NextCursor)
	assert.Equal(t, 3, res.NextCursor.MsgID)
}

// TestProcessGlobalHistoryPartialPageNoCursor: a partial raw page (fewer
// entries than limit) is the genuine end-of-stream signal; no cursor should
// be emitted.
func TestProcessGlobalHistoryPartialPageNoCursor(t *testing.T) {
	p := &Provider{}
	peer := &tg.PeerUser{UserID: 42}
	users := []tg.UserClass{&tg.User{ID: 42, FirstName: "Alice", AccessHash: 111}}

	history := &tg.MessagesMessages{
		Messages: []tg.MessageClass{
			newTestMessage(1, peer, &tg.PeerUser{UserID: 42}, "only"),
		},
		Users: users,
	}
	res, err := p.processGlobalHistory(history, 50)
	require.NoError(t, err)
	assert.False(t, res.HasMore)
	assert.Nil(t, res.NextCursor)
}

// TestProcessGlobalHistoryNextRate verifies that the next_rate hint from
// MessagesMessagesSlice is copied into the cursor when present.
func TestProcessGlobalHistoryNextRate(t *testing.T) {
	p := &Provider{}
	peer := &tg.PeerUser{UserID: 42}
	users := []tg.UserClass{&tg.User{ID: 42, FirstName: "Alice", AccessHash: 111}}

	raw := make([]tg.MessageClass, 3)
	for i := range raw {
		raw[i] = newTestMessage(i+1, peer, &tg.PeerUser{UserID: 42}, "x")
	}
	slice := &tg.MessagesMessagesSlice{
		Count:    500,
		Messages: raw,
		Users:    users,
	}
	slice.SetNextRate(777)

	res, err := p.processGlobalHistory(slice, 3)
	require.NoError(t, err)
	require.NotNil(t, res.NextCursor)
	assert.Equal(t, 777, res.NextCursor.Rate)
}

// TestProcessGlobalHistoryCursorRejectsMissingAccessHash ensures the
// constructor validation fires when a user/channel peer somehow lacks an
// access_hash — this is a programmer-error guard that prevents emitting
// cursors the wire parser would reject.
func TestProcessGlobalHistoryCursorRejectsMissingAccessHash(t *testing.T) {
	p := &Provider{}
	peer := &tg.PeerUser{UserID: 42}
	// Deliberately omit AccessHash from the user record so extractGlobalChat
	// pulls 0 for accessHash.
	users := []tg.UserClass{&tg.User{ID: 42, FirstName: "Alice"}}

	raw := make([]tg.MessageClass, 3)
	for i := range raw {
		raw[i] = newTestMessage(i+1, peer, &tg.PeerUser{UserID: 42}, "x")
	}
	history := &tg.MessagesMessagesSlice{Count: 100, Messages: raw, Users: users}

	_, err := p.processGlobalHistory(history, 3)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "access_hash")
}

// TestNewGlobalSearchCursor covers the constructor invariants directly.
func TestNewGlobalSearchCursor(t *testing.T) {
	tests := []struct {
		name       string
		kind       PeerKind
		peerID     int64
		accessHash int64
		msgID      int
		wantErr    string
	}{
		{name: "valid user", kind: PeerKindUser, peerID: 1, accessHash: 99, msgID: 1},
		{name: "valid channel", kind: PeerKindChannel, peerID: 1, accessHash: 99, msgID: 1},
		{name: "valid basic chat (no hash)", kind: PeerKindChat, peerID: 1, accessHash: 0, msgID: 1},
		{name: "user missing hash", kind: PeerKindUser, peerID: 1, accessHash: 0, msgID: 1, wantErr: "access_hash"},
		{name: "channel missing hash", kind: PeerKindChannel, peerID: 1, accessHash: 0, msgID: 1, wantErr: "access_hash"},
		{name: "unknown kind", kind: "bot", peerID: 1, accessHash: 99, msgID: 1, wantErr: "unknown peer kind"},
		{name: "zero peer id", kind: PeerKindUser, peerID: 0, accessHash: 99, msgID: 1, wantErr: "peer id"},
		{name: "zero msg id", kind: PeerKindUser, peerID: 1, accessHash: 99, msgID: 0, wantErr: "message id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewGlobalSearchCursor(0, tt.kind, tt.peerID, tt.accessHash, tt.msgID)
			if tt.wantErr == "" {
				require.NoError(t, err)
				require.NotNil(t, got)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestProcessGlobalHistoryNotModified verifies that a MessagesNotModified
// response (Telegram's "nothing changed" signal) returns an empty result
// rather than an error.
func TestProcessGlobalHistoryNotModified(t *testing.T) {
	p := &Provider{}

	res, err := p.processGlobalHistory(&tg.MessagesMessagesNotModified{}, 10)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Empty(t, res.Messages)
	assert.False(t, res.HasMore)
}

// TestSearchDateInversion verifies that Search returns an error when MinDate
// is after MaxDate without making any Telegram API call (nil client is safe
// because the guard fires before ResolvePeer).
func TestSearchDateInversion(t *testing.T) {
	p := NewProvider(nil)
	later := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	earlier := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	_, err := p.Search(t.Context(), 1, SearchOptions{
		Query:   "hello",
		MinDate: later,
		MaxDate: earlier,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "min_date")
	assert.Contains(t, err.Error(), "after max_date")
}

// TestSearchGlobalDateInversion verifies the same guard in SearchGlobal.
func TestSearchGlobalDateInversion(t *testing.T) {
	p := NewProvider(nil)
	later := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	earlier := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	_, err := p.SearchGlobal(t.Context(), GlobalSearchOptions{
		Query:   "hello",
		MinDate: later,
		MaxDate: earlier,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "min_date")
	assert.Contains(t, err.Error(), "after max_date")
}
