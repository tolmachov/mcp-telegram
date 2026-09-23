package tgdata

import (
	"context"
	"errors"
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

		// refresh=true forces a fresh fetch and mints a new snapshot ID; a
		// cursor issued against the previous snapshot keeps reading it.
		snap3, err := c.Load(t.Context(), nil, true)
		require.NoError(t, err)
		assert.Equal(t, int64(2), l.calls.Load())
		assert.NotEqual(t, snap1.ID, snap3.ID, "refresh must mint a fresh snapshot ID")

		got, ok := c.Snapshot(snap1.ID)
		require.True(t, ok, "a cursor survives another reader's refresh")
		assert.Same(t, snap1, got)
		got, ok = c.Snapshot(snap3.ID)
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

	t.Run("retained snapshots are bounded in age and number", func(t *testing.T) {
		l := &countingLoader{}
		c := NewChatsCache(l.load)
		first, err := c.Load(t.Context(), nil, false)
		require.NoError(t, err)

		first.loadedAt = time.Now().Add(-chatsCursorTTL)
		_, ok := c.Snapshot(first.ID)
		assert.False(t, ok, "a snapshot older than the cursor TTL expires")

		ids := make([]int64, 0, chatsCursorSnapshots+1)
		for range chatsCursorSnapshots + 1 {
			snap, err := c.Load(t.Context(), nil, true)
			require.NoError(t, err)
			ids = append(ids, snap.ID)
		}
		assert.Len(t, c.snaps, chatsCursorSnapshots, "the expired snapshot is dropped and the count capped")
		_, ok = c.Snapshot(ids[0])
		assert.False(t, ok, "the oldest snapshot is pushed out")
		for _, id := range ids[1:] {
			_, ok := c.Snapshot(id)
			assert.True(t, ok)
		}
	})

	t.Run("loader error is not cached", func(t *testing.T) {
		calls := 0
		c := NewChatsCache(func(context.Context, ProgressFunc) (*ChatsList, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("boom")
			}
			return &ChatsList{Chats: []ChatInfo{{ID: 1}}}, nil
		})
		_, err := c.Load(t.Context(), nil, false)
		require.EqualError(t, err, "boom")
		snap, err := c.Load(t.Context(), nil, false)
		require.NoError(t, err, "the failure must not be remembered")
		assert.Len(t, snap.Chats, 1)
		assert.Equal(t, 2, calls)
	})

	t.Run("shared load outlives the caller that started it", func(t *testing.T) {
		l := newGatedLoader()
		c := NewChatsCache(l.load)

		firstCtx, cancelFirst := context.WithCancel(t.Context())
		firstErr := make(chan error, 1)
		go func() {
			_, err := c.Load(firstCtx, nil, false)
			firstErr <- err
		}()
		<-l.entered

		second := make(chan *ChatsSnapshot, 1)
		go func() {
			snap, err := c.Load(t.Context(), func(int, string) {}, false)
			assert.NoError(t, err)
			second <- snap
		}()
		require.Eventually(t, func() bool { return c.watchers() == 1 }, time.Second, time.Millisecond, "the second caller joins the load")

		cancelFirst()
		require.ErrorIs(t, <-firstErr, context.Canceled, "the cancelled caller stops waiting")
		close(l.release)
		snap := <-second
		require.NotNil(t, snap, "the other caller still gets the snapshot")
		assert.Len(t, snap.Chats, 1)
		assert.Equal(t, int64(1), l.calls.Load())
	})

	t.Run("refresh does not join a load that started before it", func(t *testing.T) {
		l := newGatedLoader()
		c := NewChatsCache(l.load)

		stale := make(chan *ChatsSnapshot, 1)
		go func() {
			snap, err := c.Load(t.Context(), nil, false)
			assert.NoError(t, err)
			stale <- snap
		}()
		<-l.entered

		refreshed := make(chan *ChatsSnapshot, 1)
		go func() {
			snap, err := c.Load(t.Context(), nil, true)
			assert.NoError(t, err)
			refreshed <- snap
		}()
		// Give the refresh time to reach the running load. Nothing marks
		// that moment; arriving late only makes the check pass trivially.
		time.Sleep(20 * time.Millisecond)
		close(l.release)

		first, second := <-stale, <-refreshed
		require.NotNil(t, first)
		require.NotNil(t, second)
		assert.NotEqual(t, first.ID, second.ID, "the refresh gets a load of its own")
		assert.Equal(t, int64(2), l.calls.Load())
	})

	t.Run("progress reaches only the callers still waiting", func(t *testing.T) {
		l := newGatedLoader()
		c := NewChatsCache(l.load)
		var firstHeard, secondHeard atomic.Int64

		firstCtx, cancelFirst := context.WithCancel(t.Context())
		firstDone := make(chan struct{})
		go func() {
			defer close(firstDone)
			_, _ = c.Load(firstCtx, func(int, string) { firstHeard.Add(1) }, false)
		}()
		<-l.entered
		secondDone := make(chan struct{})
		go func() {
			defer close(secondDone)
			_, err := c.Load(t.Context(), func(int, string) { secondHeard.Add(1) }, false)
			assert.NoError(t, err)
		}()
		require.Eventually(t, func() bool { return c.watchers() == 2 }, time.Second, time.Millisecond)

		cancelFirst()
		<-firstDone
		close(l.release)
		<-secondDone
		assert.Zero(t, firstHeard.Load(), "a caller that left hears no more progress")
		assert.Equal(t, int64(1), secondHeard.Load(), "a caller that joined late still hears the load's progress")
	})
}

// gatedLoader reports progress once release closes, then returns a fixed
// listing; entered closes when its first call starts.
type gatedLoader struct {
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedLoader() *gatedLoader {
	return &gatedLoader{entered: make(chan struct{}), release: make(chan struct{})}
}

func (l *gatedLoader) load(ctx context.Context, onProgress ProgressFunc) (*ChatsList, error) {
	l.calls.Add(1)
	l.once.Do(func() { close(l.entered) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.release:
	}
	onProgress(1, "loaded")
	return &ChatsList{Chats: []ChatInfo{{ID: 1, Name: "A"}}, Count: 1}, nil
}

// watchers counts the callers subscribed to the running load's progress.
func (c *ChatsCache) watchers() int {
	c.mu.Lock()
	f := c.flight
	c.mu.Unlock()
	if f == nil {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.watchers)
}
