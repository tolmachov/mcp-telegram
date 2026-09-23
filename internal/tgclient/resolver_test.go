package tgclient

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func channelClient(calls *atomic.Int32, accessHash func() int64) *tg.Client {
	return tg.NewClient(fakeInvoker{
		channels: func(ids []tg.InputChannelClass) (tg.MessagesChatsClass, error) {
			calls.Add(1)
			id := ids[0].(*tg.InputChannel).ChannelID
			return &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Channel{ID: id, AccessHash: accessHash()}}}, nil
		},
	})
}

// TestResolverCachesSuccess verifies a resolved peer is served from the cache
// on the second call without a second round of MTProto probes, even when many
// callers race for the same cold ID.
func TestResolverCachesSuccess(t *testing.T) {
	var calls atomic.Int32
	r := NewResolver(channelClient(&calls, func() int64 { return 999 }), 1000)
	want := &tg.InputPeerChannel{ChannelID: 1555091578, AccessHash: 999}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			p, err := r.Resolve(t.Context(), 1555091578)
			assert.NoError(t, err)
			assert.Equal(t, want, p.Input)
		})
	}
	wg.Wait()

	p, err := r.Resolve(t.Context(), 1555091578)
	require.NoError(t, err)
	assert.Equal(t, want, p.Input)
	assert.LessOrEqual(t, calls.Load(), int32(8), "cold resolves must not multiply")
	before := calls.Load()
	_, err = r.Resolve(t.Context(), 1555091578)
	require.NoError(t, err)
	assert.Equal(t, before, calls.Load(), "a warm Resolve must hit the cache, not the API")
}

// TestResolverDoesNotCacheErrors is the core contract: a transient failure
// (e.g. FLOOD_WAIT) must be retried on the next call, never remembered as a
// permanent negative. The invoker fails once, then succeeds.
func TestResolverDoesNotCacheErrors(t *testing.T) {
	channelCalls := 0
	client := tg.NewClient(fakeInvoker{
		channels: func(ids []tg.InputChannelClass) (tg.MessagesChatsClass, error) {
			channelCalls++
			if channelCalls == 1 {
				return nil, tgerr.New(420, "FLOOD_WAIT_30")
			}
			return &tg.MessagesChats{Chats: []tg.ChatClass{
				&tg.Channel{ID: 1555091578, AccessHash: 999},
			}}, nil
		},
	})
	r := NewResolver(client, 1000)

	_, err := r.Resolve(t.Context(), 1555091578)
	var pe *PeerError
	require.ErrorAs(t, err, &pe, "first Resolve should surface the flood-wait as a PeerError")
	assert.Equal(t, int64(1555091578), pe.ID)

	p, err := r.Resolve(t.Context(), 1555091578)
	require.NoError(t, err, "the error must not have been cached; retry should succeed")
	assert.Equal(t, &tg.InputPeerChannel{ChannelID: 1555091578, AccessHash: 999}, p.Input)
	assert.Equal(t, 2, channelCalls, "the failed resolve must not be memoized")
}

// TestResolverExpiresAndEvicts covers the TTL and the bound on the cache.
func TestResolverExpiresAndEvicts(t *testing.T) {
	var calls atomic.Int32
	r := NewResolver(channelClient(&calls, func() int64 { return 1 }), 1000)
	now := time.Unix(0, 0)
	r.now = func() time.Time { return now }

	_, err := r.Resolve(t.Context(), 1)
	require.NoError(t, err)
	now = now.Add(peerCacheTTL)
	_, err = r.Resolve(t.Context(), 1)
	require.NoError(t, err)
	assert.Equal(t, int32(2), calls.Load(), "an expired entry must be resolved afresh")

	for id := int64(2); id <= peerCacheMaxEntries+1; id++ {
		r.store(id, Peer{Input: &tg.InputPeerChat{ChatID: id}})
	}
	assert.Len(t, r.byID, peerCacheMaxEntries)
}

// TestWithPeerRetriesStaleHashOnce verifies a stale access hash invalidates
// the cached peer (and the related ones) and retries exactly once with a
// fresh resolve.
func TestWithPeerRetriesStaleHashOnce(t *testing.T) {
	var calls atomic.Int32
	hash := int64(1)
	r := NewResolver(channelClient(&calls, func() int64 { return hash }), 1000)
	r.store(7, Peer{Input: &tg.InputPeerChat{ChatID: 7}})

	var seen []int64
	got, err := WithPeer(t.Context(), r, 5, []int64{7}, nil, func(p Peer) (int64, error) {
		h := p.Input.(*tg.InputPeerChannel).AccessHash
		seen = append(seen, h)
		if h == 1 {
			hash = 2
			return 0, tgerr.New(400, "CHANNEL_INVALID")
		}
		return h, nil
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), got)
	assert.Equal(t, []int64{1, 2}, seen)
	_, cached := r.cached(7)
	assert.False(t, cached, "related peers are invalidated too")

	_, err = WithPeer(t.Context(), r, 5, nil, nil, func(Peer) (int, error) {
		return 0, tgerr.New(400, "CHANNEL_INVALID")
	})
	require.Error(t, err, "the second stale answer is returned, not retried again")
}

// TestWithPeerKeepsProgress verifies a failed attempt whose result holds
// progress is returned instead of retried.
func TestWithPeerKeepsProgress(t *testing.T) {
	var calls atomic.Int32
	r := NewResolver(channelClient(&calls, func() int64 { return 1 }), 1000)
	attempts := 0
	got, err := WithPeer(t.Context(), r, 5, nil, func(n int) bool { return n > 0 }, func(Peer) (int, error) {
		attempts++
		return 3, tgerr.New(400, "PEER_ID_INVALID")
	})
	require.Error(t, err)
	assert.Equal(t, 3, got)
	assert.Equal(t, 1, attempts)
}

func TestResolverWaitHonoursContext(t *testing.T) {
	r := NewResolver(nil, 1)
	require.NoError(t, r.Wait(t.Context()))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Error(t, r.Wait(ctx))
}
