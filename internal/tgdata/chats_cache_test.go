package tgdata

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingLoader returns a fixed listing and counts how many times the network
// would have been hit. When entered/release are set, the first call blocks
// until release is closed so concurrent callers can pile up behind it.
type countingLoader struct {
	calls     atomic.Int64
	truncated bool
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (l *countingLoader) load(context.Context, ProgressFunc) (*ChatsList, error) {
	l.calls.Add(1)
	if l.entered != nil {
		l.once.Do(func() { close(l.entered) })
		<-l.release
	}
	return &ChatsList{Chats: []ChatInfo{{ID: 1, Name: "A"}}, Count: 1, Truncated: l.truncated}, nil
}

func TestChatsCacheLoad(t *testing.T) {
	t.Run("cold load fetches, warm load reuses, refresh re-fetches", func(t *testing.T) {
		l := &countingLoader{truncated: true}
		c := NewChatsCache(l.load)

		snap1, err := c.Load(t.Context(), nil, false)
		require.NoError(t, err)
		assert.NotZero(t, snap1.ID, "snapshot ID must be non-zero to distinguish an unloaded cache")
		assert.True(t, snap1.Truncated, "truncation flag must be surfaced")
		assert.Len(t, snap1.Chats, 1)
		assert.Equal(t, int64(1), l.calls.Load())

		snap2, err := c.Load(t.Context(), nil, false)
		require.NoError(t, err)
		assert.Same(t, snap1, snap2)
		assert.Equal(t, int64(1), l.calls.Load(), "warm cache must not re-paginate")

		// refresh=true forces a fresh fetch and mints a new snapshot ID, which
		// invalidates any cursor issued against the previous snapshot.
		snap3, err := c.Load(t.Context(), nil, true)
		require.NoError(t, err)
		assert.Equal(t, int64(2), l.calls.Load())
		assert.NotEqual(t, snap1.ID, snap3.ID, "refresh must mint a fresh snapshot ID")

		_, ok := c.Snapshot(snap1.ID)
		assert.False(t, ok, "a replaced snapshot must no longer be served")
		got, ok := c.Snapshot(snap3.ID)
		require.True(t, ok)
		assert.Same(t, snap3, got)
	})

	t.Run("stale snapshot is re-fetched", func(t *testing.T) {
		l := &countingLoader{}
		c := NewChatsCache(l.load)
		snap, err := c.Load(t.Context(), nil, false)
		require.NoError(t, err)

		snap.loadedAt = time.Now().Add(-ChatsMaxAge)
		fresh, err := c.Load(t.Context(), nil, false)
		require.NoError(t, err)
		assert.Equal(t, int64(2), l.calls.Load())
		assert.NotEqual(t, snap.ID, fresh.ID)
	})

	t.Run("unloaded cache has no snapshot", func(t *testing.T) {
		_, ok := NewChatsCache(nil).Snapshot(0)
		assert.False(t, ok)
	})

	t.Run("concurrent cold loads are singleflighted", func(t *testing.T) {
		l := &countingLoader{entered: make(chan struct{}), release: make(chan struct{})}
		c := NewChatsCache(l.load)
		const callers = 8
		var wg sync.WaitGroup
		ids := make(chan int64, callers)
		for range callers {
			wg.Go(func() {
				snap, err := c.Load(t.Context(), nil, false)
				assert.NoError(t, err)
				if snap != nil {
					ids <- snap.ID
				}
			})
		}
		<-l.entered
		close(l.release)
		wg.Wait()
		close(ids)
		assert.Equal(t, int64(1), l.calls.Load())
		var first int64
		for id := range ids {
			if first == 0 {
				first = id
			}
			assert.Equal(t, first, id)
		}
	})
}
