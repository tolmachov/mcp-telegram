package tgdata

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

const (
	// chatsMaxAge bounds how old the newest snapshot may grow before a
	// non-refreshing read starts a new load in the background. Completion
	// reads on every keystroke, so the listing must be cached, yet a chat
	// joined a minute ago should soon be suggested.
	chatsMaxAge = 30 * time.Second
	// chatsCursorTTL bounds how long after its load a snapshot stays readable
	// through a GetChats cursor.
	chatsCursorTTL = 30 * time.Minute
	// chatsCursorSnapshots bounds how many snapshots stay readable through
	// GetChats cursors: newer loads by other readers must not end a
	// pagination in progress, but neither may they pile up.
	chatsCursorSnapshots = 8
	// chatsLoadTimeout bounds one shared load, which runs on the cache's
	// lifetime rather than on any of the callers waiting on it.
	chatsLoadTimeout = 10 * time.Minute
)

// ChatsLoader fetches the full dialog listing (GetChats in production).
type ChatsLoader func(ctx context.Context, onProgress ProgressFunc) (*ChatsList, error)

// ChatsSnapshot is one immutable fetch of the full dialog listing. ID is
// non-zero and unique per fetch; opaque GetChats cursors name it, and
// ChatsCache.Snapshot serves it to them while it is kept (ChatsCache.Keep).
// Chats is shared by every reader and must be treated as read-only.
type ChatsSnapshot struct {
	ID        int64
	Chats     []ChatInfo
	Truncated bool
	loadedAt  time.Time
}

// ChatsCache holds the snapshots of the dialog listing that GetChats,
// SearchChats, the telegram://chats resource and completion all read, so none
// of them re-paginates every dialog on its own. The newest snapshot answers
// Load; the few a GetChats cursor names stay readable through Snapshot, so a
// cursor keeps paging the listing it started on while other readers refresh.
// Safe for concurrent use.
type ChatsCache struct {
	// life is the lifetime of the cache's owner; loads run on it.
	life context.Context
	load ChatsLoader

	mu sync.Mutex
	// newest is the newest snapshot, or nil before the first load succeeds.
	newest *ChatsSnapshot
	// kept holds the snapshots a cursor names (Keep), least recently kept
	// first.
	kept []*ChatsSnapshot
	// flight is the load in progress, if any; started counts the loads ever
	// started and numbers them, and lastStart is when the latest one started.
	flight    *chatsFlight
	started   int64
	lastStart time.Time
}

// chatsFlight is one load shared by every caller waiting on it.
type chatsFlight struct {
	seq  int64
	done chan struct{}
	// snap and err are set before done closes.
	snap *ChatsSnapshot
	err  error

	mu sync.Mutex
	// watchers are the callers subscribed to the load's progress, in the
	// order they joined.
	watchers []*progressWatcher
}

// progressWatcher is one caller subscribed to a load's progress. mu is held
// for each call to onProgress and to mark the caller gone, so no call starts
// once left is set and leaving waits out only this caller's own call.
type progressWatcher struct {
	onProgress ProgressFunc

	mu   sync.Mutex
	left bool
}

// NewChatsCache creates a cache that fills itself through load. life is the
// lifetime of the cache's owner: every load runs on it, so ending it cancels
// a load in progress and fails whoever waits on it.
func NewChatsCache(life context.Context, load ChatsLoader) *ChatsCache {
	return &ChatsCache{life: life, load: load}
}

// Load returns the newest snapshot. It waits for a new one when the cache is
// empty or refresh is true; a refresh is only satisfied by a load that starts
// after the call, never by one already running. Otherwise it answers at once,
// and a snapshot older than chatsMaxAge starts a load in the background for
// the reads after it — unless one is running or started within chatsMaxAge,
// so a failing load is not retried on every read.
//
// Concurrent fetches share one load. It belongs to none of its callers: it
// runs on the cache's lifetime (see NewChatsCache) bounded by
// chatsLoadTimeout, never on a caller's context or its values; each caller
// waits on its own ctx, and onProgress hears the load's progress only while
// its caller waits: Load does not return while onProgress runs, so only an
// onProgress that honours ctx lets a cancelled caller leave promptly.
func (c *ChatsCache) Load(ctx context.Context, onProgress ProgressFunc, refresh bool) (*ChatsSnapshot, error) {
	f, snap, err := c.join(ctx, refresh)
	if f == nil {
		return snap, err
	}
	defer f.watch(onProgress)()
	return f.wait(ctx)
}

// join returns the newest snapshot when Load may serve it without a load, or
// else the load to wait on, starting one when none that fits is running.
func (c *ChatsCache) join(ctx context.Context, refresh bool) (*chatsFlight, *ChatsSnapshot, error) {
	c.mu.Lock()
	// The first load a refresh may use is the next one to start.
	minSeq := int64(0)
	if refresh {
		minSeq = c.started + 1
	}
	for {
		if snap := c.newest; !refresh && snap != nil {
			if c.flight == nil && time.Since(snap.loadedAt) >= chatsMaxAge && time.Since(c.lastStart) >= chatsMaxAge {
				c.startLocked()
			}
			c.mu.Unlock()
			return nil, snap, nil
		}
		f := c.flight
		if f == nil {
			f = c.startLocked()
		}
		if f.seq >= minSeq {
			c.mu.Unlock()
			return f, nil, nil
		}
		// The running load predates this refresh: let it finish, then start
		// or join a newer one.
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("loading chats: %w", ctx.Err())
		case <-f.done:
		}
		c.mu.Lock()
	}
}

// Keep keeps snap readable through Snapshot, for the cursor about to name
// it: for chatsCursorTTL after its load, while it is among the
// chatsCursorSnapshots most recently kept. A snapshot no cursor names is
// never retained past the next load.
func (c *ChatsCache) Keep(snap *ChatsSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kept = slices.DeleteFunc(c.kept, func(s *ChatsSnapshot) bool {
		return s == snap || time.Since(s.loadedAt) >= chatsCursorTTL
	})
	c.kept = append(c.kept, snap)
	if excess := len(c.kept) - chatsCursorSnapshots; excess > 0 {
		c.kept = slices.Delete(c.kept, 0, excess)
	}
}

// Snapshot returns the kept snapshot with ID id. ok is false once it has aged
// out or been pushed out by newer kept ones, or when it never existed (e.g. a
// cursor from before a server restart).
func (c *ChatsCache) Snapshot(id int64) (*ChatsSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, snap := range c.kept {
		if snap.ID == id && time.Since(snap.loadedAt) < chatsCursorTTL {
			return snap, true
		}
	}
	return nil, false
}

// startLocked starts a new load. c.mu must be held.
func (c *ChatsCache) startLocked() *chatsFlight {
	c.started++
	c.lastStart = time.Now()
	f := &chatsFlight{seq: c.started, done: make(chan struct{})}
	c.flight = f
	go c.run(f)
	return f
}

// run performs the load for f on the cache's lifetime and publishes its
// snapshot as the newest.
func (c *ChatsCache) run(f *chatsFlight) {
	ctx, cancel := context.WithTimeout(c.life, chatsLoadTimeout)
	defer cancel()
	result, err := c.load(ctx, f.progress)

	c.mu.Lock()
	if err != nil {
		f.err = err
	} else {
		f.snap = &ChatsSnapshot{
			ID:        tgclient.RandomID(),
			Chats:     result.Chats,
			Truncated: result.Truncated,
			loadedAt:  time.Now(),
		}
		c.newest = f.snap
	}
	c.flight = nil
	c.mu.Unlock()
	close(f.done)
}

// watch subscribes onProgress to f's progress until the returned func is
// called; once it returns, onProgress is not called again. A nil onProgress
// hears nothing.
func (f *chatsFlight) watch(onProgress ProgressFunc) func() {
	if onProgress == nil {
		return func() {}
	}
	w := &progressWatcher{onProgress: onProgress}
	f.mu.Lock()
	f.watchers = append(f.watchers, w)
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.watchers = slices.DeleteFunc(f.watchers, func(o *progressWatcher) bool { return o == w })
		f.mu.Unlock()
		w.mu.Lock()
		w.left = true
		w.mu.Unlock()
	}
}

// progress relays the load's progress to the callers waiting on it. The
// callbacks write to the network, so they run outside f.mu: holding it would
// stall every caller joining or leaving the load behind one slow write. A
// caller that leaves after the watchers are copied is skipped by its left
// flag.
func (f *chatsFlight) progress(current int, message string) {
	f.mu.Lock()
	watchers := slices.Clone(f.watchers)
	f.mu.Unlock()
	for _, w := range watchers {
		w.relay(current, message)
	}
}

// relay calls w's onProgress unless its caller has left.
func (w *progressWatcher) relay(current int, message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.left {
		w.onProgress(current, message)
	}
}

// wait returns f's outcome, or ctx's error if ctx ends first.
func (f *chatsFlight) wait(ctx context.Context) (*ChatsSnapshot, error) {
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("loading chats: %w", ctx.Err())
	case <-f.done:
		return f.snap, f.err
	}
}
