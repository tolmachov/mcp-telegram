package tools

import (
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// chatsCacheInvoker fakes messages.getDialogs, returning an empty (single-page)
// dialog list and counting how many times the network was actually hit.
type chatsCacheInvoker struct {
	calls int
}

func (f *chatsCacheInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	if _, ok := input.(*tg.MessagesGetDialogsRequest); ok {
		f.calls++
		output.(*tg.MessagesDialogsBox).Dialogs = &tg.MessagesDialogs{}
		return nil
	}
	return fmt.Errorf("chatsCacheInvoker: unexpected request %T", input)
}

func TestChatsCacheLoad(t *testing.T) {
	t.Run("warm cache serves without fetching", func(t *testing.T) {
		// A nil client would panic if load tried to fetch, so reaching the
		// cached return proves no network call happened.
		c := &ChatsCache{
			chats:     []tgdata.ChatInfo{{ID: 1, Name: "A"}},
			sessionID: 42,
			truncated: true,
		}
		chats, sid, truncated, err := c.load(context.Background(), nil, false)
		require.NoError(t, err)
		assert.Equal(t, int64(42), sid)
		assert.True(t, truncated, "cached truncation flag must be surfaced")
		assert.Len(t, chats, 1)
	})

	t.Run("cold load fetches, warm load reuses, refresh re-fetches", func(t *testing.T) {
		inv := &chatsCacheInvoker{}
		c := NewChatsCache(tg.NewClient(inv))

		_, sid1, truncated, err := c.load(context.Background(), nil, false)
		require.NoError(t, err)
		assert.NotZero(t, sid1, "session ID must be non-zero to distinguish an unloaded cache")
		assert.False(t, truncated)
		assert.Equal(t, 1, inv.calls)

		// Warm cache: no refresh, so no second network call and a stable session.
		_, sid2, _, err := c.load(context.Background(), nil, false)
		require.NoError(t, err)
		assert.Equal(t, sid1, sid2)
		assert.Equal(t, 1, inv.calls, "warm cache must not re-paginate")

		// refresh=true forces a fresh fetch and mints a new session ID, which
		// invalidates any cursor issued against the previous snapshot.
		_, sid3, _, err := c.load(context.Background(), nil, true)
		require.NoError(t, err)
		assert.Equal(t, 2, inv.calls)
		assert.NotEqual(t, sid1, sid3, "refresh must mint a fresh session ID")
	})
}
