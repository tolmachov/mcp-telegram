package tools

import (
	"context"
	"sync"

	"github.com/gotd/td/tg"

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

	result, err := tgdata.GetChats(ctx, c.client, onProgress)
	if err != nil {
		return nil, 0, false, err
	}
	sid := cryptoRandInt64() | 1 // guarantee non-zero to distinguish an unloaded cache

	c.mu.Lock()
	c.chats = result.Chats
	c.sessionID = sid
	c.truncated = result.Truncated
	c.mu.Unlock()

	return result.Chats, sid, result.Truncated, nil
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
