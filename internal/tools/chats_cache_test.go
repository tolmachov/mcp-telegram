package tools

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
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
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *chatsCacheInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	if _, ok := input.(*tg.MessagesGetDialogsRequest); ok {
		f.calls.Add(1)
		if f.entered != nil {
			f.once.Do(func() { close(f.entered) })
			<-f.release
		}
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
		assert.Equal(t, int64(1), inv.calls.Load())

		// Warm cache: no refresh, so no second network call and a stable session.
		_, sid2, _, err := c.load(context.Background(), nil, false)
		require.NoError(t, err)
		assert.Equal(t, sid1, sid2)
		assert.Equal(t, int64(1), inv.calls.Load(), "warm cache must not re-paginate")

		// refresh=true forces a fresh fetch and mints a new session ID, which
		// invalidates any cursor issued against the previous snapshot.
		_, sid3, _, err := c.load(context.Background(), nil, true)
		require.NoError(t, err)
		assert.Equal(t, int64(2), inv.calls.Load())
		assert.NotEqual(t, sid1, sid3, "refresh must mint a fresh session ID")
	})

	t.Run("concurrent cold loads are singleflighted", func(t *testing.T) {
		inv := &chatsCacheInvoker{entered: make(chan struct{}), release: make(chan struct{})}
		c := NewChatsCache(tg.NewClient(inv))
		const callers = 8
		var wg sync.WaitGroup
		sids := make(chan int64, callers)
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, sid, _, err := c.load(t.Context(), nil, false)
				require.NoError(t, err)
				sids <- sid
			}()
		}
		<-inv.entered
		close(inv.release)
		wg.Wait()
		close(sids)
		assert.Equal(t, int64(1), inv.calls.Load())
		var first int64
		for sid := range sids {
			if first == 0 {
				first = sid
			}
			assert.Equal(t, first, sid)
		}
	})
}
