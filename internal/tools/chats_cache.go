package tools

import (
	"context"
	"sync"

	"github.com/gotd/td/tg"
	"golang.org/x/sync/singleflight"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// ChatsCache holds one shared snapshot of the full dialog listing so that
// GetChats and SearchChats don't each re-paginate every dialog on Telegram
// independently. Before this, GetChats cached its own copy while SearchChats
// re-fetched the entire list on every call. A snapshot carries a non-zero
// session ID that opaque GetChats cursors reference, so a refresh invalidates
// any outstanding cursor. Safe for concurrent use.
type ChatsCache struct {
	client    *tg.Client
	mu        sync.RWMutex
	chats     []tgdata.ChatInfo
	sessionID int64
	truncated bool
	loads     singleflight.Group
}

// NewChatsCache creates a cache backed by the given client.
func NewChatsCache(client *tg.Client) *ChatsCache {
	return &ChatsCache{client: client}
}

// load returns the cached snapshot, fetching a fresh listing from Telegram when
// the cache is empty or refresh is true. It returns the chats, the session ID
// identifying that snapshot, and whether that snapshot is truncated (dialog
// pagination stalled — see tgdata.ChatsList.Truncated). The returned slice is
// shared with the cache and later readers; callers must treat it as read-only.
func (c *ChatsCache) load(ctx context.Context, onProgress tgdata.ProgressFunc, refresh bool) ([]tgdata.ChatInfo, int64, bool, error) {
	if !refresh {
		c.mu.RLock()
		chats, sid, truncated := c.chats, c.sessionID, c.truncated
		c.mu.RUnlock()
		if sid != 0 {
			return chats, sid, truncated, nil
		}
	}

	type loadResult struct {
		chats     []tgdata.ChatInfo
		sid       int64
		truncated bool
	}
	resultCh := c.loads.DoChan("dialogs", func() (any, error) {
		if !refresh {
			c.mu.RLock()
			cached := loadResult{chats: c.chats, sid: c.sessionID, truncated: c.truncated}
			c.mu.RUnlock()
			if cached.sid != 0 {
				return cached, nil
			}
		}
		result, err := tgdata.GetChats(ctx, c.client, onProgress)
		if err != nil {
			return loadResult{}, err
		}
		loaded := loadResult{chats: result.Chats, sid: cryptoRandInt64() | 1, truncated: result.Truncated}
		c.mu.Lock()
		c.chats = loaded.chats
		c.sessionID = loaded.sid
		c.truncated = loaded.truncated
		c.mu.Unlock()
		return loaded, nil
	})
	select {
	case <-ctx.Done():
		return nil, 0, false, ctx.Err()
	case outcome := <-resultCh:
		if outcome.Err != nil {
			return nil, 0, false, outcome.Err
		}
		loaded := outcome.Val.(loadResult)
		return loaded.chats, loaded.sid, loaded.truncated, nil
	}
}

// snapshot returns the cached chats for sessionID and whether that snapshot is
// truncated. ok is false when the cache was refreshed or never loaded — i.e. the
// caller's cursor is stale. The returned slice is shared; treat it as read-only.
func (c *ChatsCache) snapshot(sessionID int64) (chats []tgdata.ChatInfo, truncated, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.sessionID == 0 || sessionID != c.sessionID {
		return nil, false, false
	}
	return c.chats, c.truncated, true
}
