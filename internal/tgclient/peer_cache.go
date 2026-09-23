package tgclient

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"golang.org/x/sync/singleflight"
)

const (
	peerCacheMaxEntries = 4096
	peerCacheTTL        = time.Hour
)

type peerCacheEntry struct {
	peer      tg.InputPeerClass
	expiresAt time.Time
}

// PeerCache is a bounded, expiring cache. Concurrent cold resolves for the
// same peer collapse into one Telegram probe.
type PeerCache struct {
	mu   sync.RWMutex
	byID map[int64]peerCacheEntry
	load singleflight.Group
	now  func() time.Time
}

func NewPeerCache() *PeerCache {
	return &PeerCache{byID: make(map[int64]peerCacheEntry), now: time.Now}
}

func (c *PeerCache) Resolve(ctx context.Context, client *tg.Client, id int64) (tg.InputPeerClass, error) {
	now := c.now()
	c.mu.RLock()
	entry, ok := c.byID[id]
	c.mu.RUnlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.peer, nil
	}

	value, err, _ := c.load.Do(strconv.FormatInt(id, 10), func() (any, error) {
		now := c.now()
		c.mu.RLock()
		entry, ok := c.byID[id]
		c.mu.RUnlock()
		if ok && now.Before(entry.expiresAt) {
			return entry.peer, nil
		}
		peer, err := ResolvePeer(ctx, client, id)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.removeExpiredLocked(now)
		if len(c.byID) >= peerCacheMaxEntries {
			for victim := range c.byID {
				delete(c.byID, victim)
				break
			}
		}
		c.byID[id] = peerCacheEntry{peer: peer, expiresAt: now.Add(peerCacheTTL)}
		c.mu.Unlock()
		return peer, nil
	})
	if err != nil {
		return nil, fmt.Errorf("resolving cached peer %d: %w", id, err)
	}
	return value.(tg.InputPeerClass), nil
}

func (c *PeerCache) removeExpiredLocked(now time.Time) {
	for id, entry := range c.byID {
		if !now.Before(entry.expiresAt) {
			delete(c.byID, id)
		}
	}
}

func (c *PeerCache) Invalidate(id int64) {
	c.mu.Lock()
	delete(c.byID, id)
	c.mu.Unlock()
}
