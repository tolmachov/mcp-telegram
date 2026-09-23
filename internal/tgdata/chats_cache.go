package tgdata

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ChatsMaxAge bounds how old a snapshot may be before a non-refreshing read
// fetches a new one. Completion reads on every keystroke, so the listing must
// be cached, yet a chat joined a minute ago should still be suggested.
const ChatsMaxAge = 30 * time.Second

// ChatsLoader fetches the full dialog listing (GetChats in production).
type ChatsLoader func(ctx context.Context, onProgress ProgressFunc) (*ChatsList, error)

// ChatsSnapshot is one immutable fetch of the full dialog listing. ID is
// non-zero and unique per fetch, so opaque GetChats cursors that reference it
// expire once a newer snapshot replaces it. Chats is shared by every reader
// and must be treated as read-only.
type ChatsSnapshot struct {
	ID        int64
	Chats     []ChatInfo
	Truncated bool
	loadedAt  time.Time
}

// ChatsCache holds the one shared snapshot of the dialog listing that GetChats,
// SearchChats, the telegram://chats resource and completion all read, so none
// of them re-paginates every dialog on its own. Safe for concurrent use.
type ChatsCache struct {
	load  ChatsLoader
	mu    sync.RWMutex
	snap  *ChatsSnapshot
	loads singleflight.Group
}

// NewChatsCache creates a cache that fills itself through load.
func NewChatsCache(load ChatsLoader) *ChatsCache {
	return &ChatsCache{load: load}
}

// Load returns the current snapshot. It fetches a new one when the cache is
// empty, when the snapshot is older than ChatsMaxAge, or when refresh is true.
// Concurrent fetches share one in-flight load.
func (c *ChatsCache) Load(ctx context.Context, onProgress ProgressFunc, refresh bool) (*ChatsSnapshot, error) {
	if snap := c.fresh(refresh); snap != nil {
		return snap, nil
	}
	resultCh := c.loads.DoChan("dialogs", func() (any, error) {
		// A load that finished while this caller waited is fresh enough.
		if snap := c.fresh(refresh); snap != nil {
			return snap, nil
		}
		result, err := c.load(ctx, onProgress)
		if err != nil {
			return nil, err
		}
		snap := &ChatsSnapshot{
			ID:        randomSnapshotID(),
			Chats:     result.Chats,
			Truncated: result.Truncated,
			loadedAt:  time.Now(),
		}
		c.mu.Lock()
		c.snap = snap
		c.mu.Unlock()
		return snap, nil
	})
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("loading chats: %w", ctx.Err())
	case outcome := <-resultCh:
		if outcome.Err != nil {
			return nil, outcome.Err
		}
		return outcome.Val.(*ChatsSnapshot), nil
	}
}

// Snapshot returns the current snapshot if its ID is id. ok is false when the
// cache was refreshed or never loaded — i.e. a cursor naming id is stale.
func (c *ChatsCache) Snapshot(id int64) (*ChatsSnapshot, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.snap == nil || c.snap.ID != id {
		return nil, false
	}
	return c.snap, true
}

// fresh returns the current snapshot when it may be served without a fetch.
func (c *ChatsCache) fresh(refresh bool) *ChatsSnapshot {
	if refresh {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.snap == nil || time.Since(c.snap.loadedAt) >= ChatsMaxAge {
		return nil
	}
	return c.snap
}

// randomSnapshotID returns a random non-zero ID, so a cursor from before a
// server restart cannot match a new snapshot.
func randomSnapshotID() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never errors on supported platforms
	//nolint:gosec // G115: intentional full-width uint64→int64 reinterpretation for a random id; every bit pattern is a valid id.
	return int64(binary.LittleEndian.Uint64(b[:])) | 1
}
