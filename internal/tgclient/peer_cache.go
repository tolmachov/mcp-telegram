package tgclient

import (
	"context"
	"sync"

	"github.com/gotd/td/tg"
)

// PeerCache memoises ResolvePeer results. A resolved InputPeer embeds the
// access_hash MTProto requires, which is stable for the lifetime of a session —
// and there is exactly one session per process — so a peer resolved once stays
// valid until the process exits. Without this, every history fetch or search
// re-runs up to three live API calls (users.getUsers → channels.getChannels →
// messages.getChats) to re-resolve a chat the caller touched seconds ago; on a
// read-heavy workflow that is the main avoidable source of request volume and
// FLOOD_WAIT risk.
//
// A nil *PeerCache is usable and simply resolves without caching, so the zero
// value and cache-less call sites need no special handling. Safe for concurrent
// use.
type PeerCache struct {
	mu   sync.RWMutex
	byID map[int64]tg.InputPeerClass
}

// NewPeerCache returns an empty peer cache.
func NewPeerCache() *PeerCache {
	return &PeerCache{byID: make(map[int64]tg.InputPeerClass)}
}

// Resolve returns the cached peer for id, or resolves it via ResolvePeer and
// caches the result. Errors are never cached: a transient failure (FLOOD_WAIT)
// or a not-yet-accessible channel must be retried on the next call, not
// remembered as a permanent negative.
func (c *PeerCache) Resolve(ctx context.Context, client *tg.Client, id int64) (tg.InputPeerClass, error) {
	if c != nil {
		c.mu.RLock()
		peer, ok := c.byID[id]
		c.mu.RUnlock()
		if ok {
			return peer, nil
		}
	}

	peer, err := ResolvePeer(ctx, client, id)
	if err != nil {
		return nil, err
	}

	if c != nil {
		c.mu.Lock()
		c.byID[id] = peer
		c.mu.Unlock()
	}
	return peer, nil
}
