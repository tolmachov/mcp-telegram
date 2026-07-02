package tgclient

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPeerCacheResolveCachesSuccess verifies a resolved peer is served from the
// cache on the second call without a second round of MTProto probes.
func TestPeerCacheResolveCachesSuccess(t *testing.T) {
	ctx := context.Background()
	channelCalls := 0
	client := tg.NewClient(fakeInvoker{
		channels: func(ids []tg.InputChannelClass) (tg.MessagesChatsClass, error) {
			channelCalls++
			return &tg.MessagesChats{Chats: []tg.ChatClass{
				&tg.Channel{ID: 1555091578, AccessHash: 999},
			}}, nil
		},
	})

	c := NewPeerCache()
	want := &tg.InputPeerChannel{ChannelID: 1555091578, AccessHash: 999}

	p1, err := c.Resolve(ctx, client, 1555091578)
	require.NoError(t, err)
	assert.Equal(t, want, p1)

	p2, err := c.Resolve(ctx, client, 1555091578)
	require.NoError(t, err)
	assert.Equal(t, want, p2)

	assert.Equal(t, 1, channelCalls, "second Resolve must hit the cache, not the API")
}

// TestPeerCacheResolveDoesNotCacheErrors is the core contract: a transient
// failure (e.g. FLOOD_WAIT) must be retried on the next call, never remembered
// as a permanent negative. The invoker fails once, then succeeds.
func TestPeerCacheResolveDoesNotCacheErrors(t *testing.T) {
	ctx := context.Background()
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

	c := NewPeerCache()

	_, err := c.Resolve(ctx, client, 1555091578)
	require.Error(t, err, "first Resolve should surface the flood-wait")

	p, err := c.Resolve(ctx, client, 1555091578)
	require.NoError(t, err, "the error must not have been cached; retry should succeed")
	assert.Equal(t, &tg.InputPeerChannel{ChannelID: 1555091578, AccessHash: 999}, p)
	assert.Equal(t, 2, channelCalls, "the failed resolve must not be memoized")
}

// TestPeerCacheNilReceiver documents that a nil *PeerCache resolves without
// caching and without panicking, so cache-less call sites need no guard.
func TestPeerCacheNilReceiver(t *testing.T) {
	ctx := context.Background()
	client := tg.NewClient(fakeInvoker{
		channels: func(ids []tg.InputChannelClass) (tg.MessagesChatsClass, error) {
			return &tg.MessagesChats{Chats: []tg.ChatClass{
				&tg.Channel{ID: 1555091578, AccessHash: 999},
			}}, nil
		},
	})

	var c *PeerCache
	p, err := c.Resolve(ctx, client, 1555091578)
	require.NoError(t, err)
	assert.Equal(t, &tg.InputPeerChannel{ChannelID: 1555091578, AccessHash: 999}, p)
}
