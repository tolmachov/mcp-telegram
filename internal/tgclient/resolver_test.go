package tgclient

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func channelClientHandler(calls *atomic.Int32, accessHash func() int64) func([]tg.InputChannelClass) (tg.MessagesChatsClass, error) {
	return func(ids []tg.InputChannelClass) (tg.MessagesChatsClass, error) {
		calls.Add(1)
		id := ids[0].(*tg.InputChannel).ChannelID
		return &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Channel{ID: id, AccessHash: accessHash()}}}, nil
	}
}

func channelClient(calls *atomic.Int32, accessHash func() int64) *tg.Client {
	return tg.NewClient(fakeInvoker{channels: channelClientHandler(calls, accessHash)})
}

// gatedInvoker holds every call until gate closes, failing it instead when
// its ctx ends first, like a real transport. entered receives one value per
// call that reached the gate.
type gatedInvoker struct {
	inner   fakeInvoker
	entered chan struct{}
	gate    chan struct{}
}

func (g gatedInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	g.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.gate:
		return g.inner.Invoke(ctx, input, output)
	}
}

// TestResolverCachesSuccess verifies concurrent cold resolves of one ID share
// a single probe, and a warm Resolve is served from the cache.
func TestResolverCachesSuccess(t *testing.T) {
	var calls atomic.Int32
	inv := gatedInvoker{
		inner:   fakeInvoker{channels: channelClientHandler(&calls, func() int64 { return 999 })},
		entered: make(chan struct{}, 16),
		gate:    make(chan struct{}),
	}
	r := NewResolver(t.Context(), tg.NewClient(inv))
	want := &tg.InputPeerChannel{ChannelID: 1555091578, AccessHash: 999}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			p, err := r.Resolve(t.Context(), 1555091578)
			assert.NoError(t, err)
			assert.Equal(t, want, p.Input)
		})
	}
	<-inv.entered // the shared probe holds at the gate while the callers pile up
	close(inv.gate)
	wg.Wait()
	assert.Equal(t, int32(1), calls.Load(), "concurrent cold resolves must share one probe")

	p, err := r.Resolve(t.Context(), 1555091578)
	require.NoError(t, err)
	assert.Equal(t, want, p.Input)
	assert.Equal(t, int32(1), calls.Load(), "a warm Resolve must hit the cache, not the API")
}

// TestResolverSharedProbeOutlivesCaller verifies the caller that started a
// shared probe giving up does not fail another caller waiting on it.
func TestResolverSharedProbeOutlivesCaller(t *testing.T) {
	var calls atomic.Int32
	inv := gatedInvoker{
		inner:   fakeInvoker{channels: channelClientHandler(&calls, func() int64 { return 999 })},
		entered: make(chan struct{}, 16),
		gate:    make(chan struct{}),
	}
	r := NewResolver(t.Context(), tg.NewClient(inv))

	firstCtx, cancelFirst := context.WithCancel(t.Context())
	firstErr := make(chan error, 1)
	go func() {
		_, err := r.Resolve(firstCtx, 5)
		firstErr <- err
	}()
	<-inv.entered // the first caller's probe is in flight

	second := make(chan error, 1)
	go func() {
		p, err := r.Resolve(t.Context(), 5)
		if err == nil {
			assert.Equal(t, &tg.InputPeerChannel{ChannelID: 5, AccessHash: 999}, p.Input)
		}
		second <- err
	}()

	cancelFirst()
	err := <-firstErr
	require.ErrorIs(t, err, context.Canceled, "the cancelled caller stops waiting")
	require.ErrorContains(t, err, "resolving chat 5")
	assert.False(t, IsPeerSpecific(err), "giving up says nothing about the chat")

	close(inv.gate)
	require.NoError(t, <-second, "the other caller still gets the peer")
	assert.Equal(t, int32(1), calls.Load())
}

// ctxKey tags a caller's context so a test can tell whether a probe saw it.
type ctxKey struct{}

// valueSpy records whether a call's context carried a ctxKey value.
type valueSpy struct {
	gatedInvoker
	sawCaller chan bool
}

func (v valueSpy) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	v.sawCaller <- ctx.Value(ctxKey{}) != nil
	return v.gatedInvoker.Invoke(ctx, input, output)
}

// TestResolverProbeRunsOnItsLifetime pins that a shared probe runs on the
// resolver's lifetime, not on its first caller's context: it carries none of
// that caller's values, and ending the lifetime (the assembly closing) cancels
// it and fails every caller waiting on it.
func TestResolverProbeRunsOnItsLifetime(t *testing.T) {
	var calls atomic.Int32
	inv := valueSpy{
		gatedInvoker: gatedInvoker{
			inner:   fakeInvoker{channels: channelClientHandler(&calls, func() int64 { return 999 })},
			entered: make(chan struct{}, 16),
			gate:    make(chan struct{}),
		},
		sawCaller: make(chan bool, 16),
	}
	life, end := context.WithCancel(t.Context())
	r := NewResolver(life, tg.NewClient(inv))

	const waiters = 3
	errs := make(chan error, waiters)
	caller := context.WithValue(t.Context(), ctxKey{}, "first caller")
	go func() {
		_, err := r.Resolve(caller, 5)
		errs <- err
	}()
	<-inv.entered
	assert.False(t, <-inv.sawCaller, "the probe must not carry its first caller's values")
	for range waiters - 1 {
		go func() {
			_, err := r.Resolve(t.Context(), 5)
			errs <- err
		}()
	}

	end()
	for range waiters {
		err := <-errs
		require.ErrorIs(t, err, context.Canceled, "ending the lifetime fails the waiters")
		assert.False(t, IsPeerSpecific(err), "the lifetime ending says nothing about the chat")
	}
	_, ok := r.cached(5)
	assert.False(t, ok, "a cancelled probe caches nothing")
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
	r := NewResolver(t.Context(), client)

	_, err := r.Resolve(t.Context(), 1555091578)
	_, isFlood := tgerr.AsFloodWait(err)
	require.True(t, isFlood, "first Resolve should surface the flood wait: %v", err)
	assert.ErrorContains(t, err, "1555091578", "the error names the chat")
	assert.False(t, IsPeerSpecific(err), "a flood wait says nothing about the chat")

	p, err := r.Resolve(t.Context(), 1555091578)
	require.NoError(t, err, "the error must not have been cached; retry should succeed")
	assert.Equal(t, &tg.InputPeerChannel{ChannelID: 1555091578, AccessHash: 999}, p.Input)
	assert.Equal(t, 2, channelCalls, "the failed resolve must not be memoised")
}

// TestResolverExpiresAndEvicts covers the TTL and the bound on the cache.
func TestResolverExpiresAndEvicts(t *testing.T) {
	var calls atomic.Int32
	r := NewResolver(t.Context(), channelClient(&calls, func() int64 { return 1 }))
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

// TestResolverEvictionPrefersExpired verifies a full cache makes room by
// dropping expired entries, keeping every live one.
func TestResolverEvictionPrefersExpired(t *testing.T) {
	r := NewResolver(t.Context(), nil)
	now := time.Unix(0, 0)
	r.now = func() time.Time { return now }

	const expired = int64(1)
	r.store(expired, Peer{Input: &tg.InputPeerChat{ChatID: expired}})
	now = now.Add(peerCacheTTL / 2)
	for id := expired + 1; id <= peerCacheMaxEntries; id++ {
		r.store(id, Peer{Input: &tg.InputPeerChat{ChatID: id}})
	}
	require.Len(t, r.byID, peerCacheMaxEntries)

	now = now.Add(peerCacheTTL / 2) // only the first entry has expired
	r.store(peerCacheMaxEntries+1, Peer{Input: &tg.InputPeerChat{ChatID: peerCacheMaxEntries + 1}})
	assert.Len(t, r.byID, peerCacheMaxEntries)
	assert.NotContains(t, r.byID, expired, "the expired entry makes room")
	for id := expired + 1; id <= peerCacheMaxEntries+1; id++ {
		assert.Contains(t, r.byID, id, "no live entry is evicted while an expired one exists")
	}
}

// TestWithPeerRetriesStaleHashOnce verifies a stale access hash invalidates
// the cached peer and retries exactly once with a fresh resolve.
func TestWithPeerRetriesStaleHashOnce(t *testing.T) {
	var calls atomic.Int32
	hash := int64(1)
	r := NewResolver(t.Context(), channelClient(&calls, func() int64 { return hash }))

	var seen []int64
	got, err := WithPeer(t.Context(), r, 5, func(p Peer) (int64, error) {
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

	_, err = WithPeer(t.Context(), r, 5, func(Peer) (int, error) {
		return 0, tgerr.New(400, "CHANNEL_INVALID")
	})
	require.Error(t, err, "the second stale answer is returned, not retried again")
}

// TestWithPeerKeepingPartial verifies a failed attempt whose result holds
// progress is returned instead of retried, and one without progress is
// retried.
func TestWithPeerKeepingPartial(t *testing.T) {
	var calls atomic.Int32
	r := NewResolver(t.Context(), channelClient(&calls, func() int64 { return 1 }))
	attempts := 0
	progressed := func(n int) bool { return n > 0 }
	got, err := WithPeerKeepingPartial(t.Context(), r, 5, progressed, func(Peer) (int, error) {
		attempts++
		return 3, tgerr.New(400, "PEER_ID_INVALID")
	})
	require.Error(t, err)
	assert.Equal(t, 3, got)
	assert.Equal(t, 1, attempts)

	attempts = 0
	got, err = WithPeerKeepingPartial(t.Context(), r, 5, progressed, func(Peer) (int, error) {
		attempts++
		if attempts == 1 {
			return 0, tgerr.New(400, "PEER_ID_INVALID")
		}
		return 4, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 4, got)
	assert.Equal(t, 2, attempts)
}

// TestWithPeersRefreshesEveryPeer verifies a stale answer re-resolves all the
// peers op was given.
func TestWithPeersRefreshesEveryPeer(t *testing.T) {
	var calls atomic.Int32
	hash := int64(1)
	r := NewResolver(t.Context(), channelClient(&calls, func() int64 { return hash }))

	var seen [][2]int64
	_, err := WithPeers(t.Context(), r, []int64{5, 6}, func(p []Peer) (bool, error) {
		pair := [2]int64{p[0].Input.(*tg.InputPeerChannel).AccessHash, p[1].Input.(*tg.InputPeerChannel).AccessHash}
		seen = append(seen, pair)
		if pair[0] == 1 {
			hash = 2
			return false, tgerr.New(400, "CHANNEL_INVALID")
		}
		return true, nil
	})
	require.NoError(t, err)
	assert.Equal(t, [][2]int64{{1, 1}, {2, 2}}, seen)
}

// TestWithPeersFromDropsStalePeers verifies a stale answer drops the peers op
// was given from the cache and runs resolve and op once more.
func TestWithPeersFromDropsStalePeers(t *testing.T) {
	var calls atomic.Int32
	hash := int64(1)
	r := NewResolver(t.Context(), channelClient(&calls, func() int64 { return hash }))
	named := Peer{Input: &tg.InputPeerUser{UserID: 9, AccessHash: 90}}

	resolves := 0
	var seen []int64
	got, err := WithPeersFrom(r, func() ([]Peer, error) {
		resolves++
		peer, err := r.Resolve(t.Context(), 5)
		return []Peer{peer, named}, err
	}, func(p []Peer) (int64, error) {
		h := p[0].Input.(*tg.InputPeerChannel).AccessHash
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
	assert.Equal(t, 2, resolves)

	peer, err := r.Resolve(t.Context(), 5)
	require.NoError(t, err)
	assert.Equal(t, int64(2), peer.Input.(*tg.InputPeerChannel).AccessHash, "the fresh peer replaces the stale one in the cache")
	assert.Equal(t, int32(2), calls.Load())
}

func TestPeerID(t *testing.T) {
	assert.Equal(t, int64(1), Peer{Input: &tg.InputPeerUser{UserID: 1}}.ID())
	assert.Equal(t, int64(2), Peer{Input: &tg.InputPeerChat{ChatID: 2}}.ID())
	assert.Equal(t, int64(3), Peer{Input: &tg.InputPeerChannel{ChannelID: 3}}.ID())
	assert.Zero(t, Peer{Input: &tg.InputPeerSelf{}}.ID())
}
