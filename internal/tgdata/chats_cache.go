package tgdata

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"slices"
	"sync"
	"time"
)

const (
	// ChatsMaxAge bounds how old the newest snapshot may be before a
	// non-refreshing read fetches a new one. Completion reads on every
	// keystroke, so the listing must be cached, yet a chat joined a minute ago
	// should still be suggested.
	ChatsMaxAge = 30 * time.Second
	// chatsCursorTTL bounds how long after its load a snapshot stays readable
	// through a GetChats cursor.
	chatsCursorTTL = 30 * time.Minute
	// chatsCursorSnapshots bounds how many snapshots stay readable through
	// GetChats cursors: newer loads by other readers must not end a
	// pagination in progress, but neither may they pile up.
	chatsCursorSnapshots = 8
	// chatsLoadTimeout bounds one shared load, which runs detached from the
	// callers waiting on it.
	chatsLoadTimeout = 10 * time.Minute
)

// ChatsLoader fetches the full dialog listing (GetChats in production).
type ChatsLoader func(ctx context.Context, onProgress ProgressFunc) (*ChatsList, error)

// ChatsSnapshot is one immutable fetch of the full dialog listing. ID is
// non-zero and unique per fetch; opaque GetChats cursors name it, and
// ChatsCache.Snapshot serves it to them while it is retained. Chats is shared
// by every reader and must be treated as read-only.
type ChatsSnapshot struct {
	ID        int64
	Chats     []ChatInfo
	Truncated bool
	loadedAt  time.Time
}

// ChatsCache holds the snapshots of the dialog listing that GetChats,
// SearchChats, the telegram://chats resource and completion all read, so none
// of them re-paginates every dialog on its own. The newest snapshot answers
// Load; the few most recent ones stay readable through Snapshot, so a GetChats
// cursor keeps paging the listing it started on while other readers refresh.
// Safe for concurrent use.
type ChatsCache struct {
	load ChatsLoader

	mu sync.Mutex
	// snaps holds the retained snapshots, oldest first; the last one is the
	// newest.
	snaps []*ChatsSnapshot
	// flight is the load in progress, if any; started counts the loads ever
	// started and numbers them.
	flight  *chatsFlight
	started int64
}

// chatsFlight is one load shared by every caller waiting on it.
type chatsFlight struct {
	seq  int64
	done chan struct{}
	// snap and err are set before done closes.
	snap *ChatsSnapshot
	err  error

	mu       sync.Mutex
	watchers map[int]ProgressFunc
	nextID   int
}

// NewChatsCache creates a cache that fills itself through load.
func NewChatsCache(load ChatsLoader) *ChatsCache {
	return &ChatsCache{load: load}
}

// Load returns the newest snapshot. It fetches a new one when the cache is
// empty, when the newest snapshot is older than ChatsMaxAge, or when refresh
// is true; a refresh is only satisfied by a load that starts after the call,
// never by one already running.
//
// Concurrent fetches share one load. It belongs to none of its callers: it
// runs on a context detached from the one that started it (bounded by
// chatsLoadTimeout), each caller waits on its own ctx, and onProgress hears
// the load's progress only while its caller waits.
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
		if snap := c.newest(); !refresh && snap != nil && time.Since(snap.loadedAt) < ChatsMaxAge {
			c.mu.Unlock()
			return nil, snap, nil
		}
		f := c.flight
		if f == nil {
			f = c.startLocked(ctx)
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

// Snapshot returns the snapshot with ID id while it is retained. ok is false
// once it has aged out or been pushed out by newer loads, or when it never
// existed (e.g. a cursor from before a server restart).
func (c *ChatsCache) Snapshot(id int64) (*ChatsSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, snap := range c.snaps {
		if snap.ID == id && time.Since(snap.loadedAt) < chatsCursorTTL {
			return snap, true
		}
	}
	return nil, false
}

// newest returns the newest snapshot, or nil for an empty cache. c.mu must be
// held.
func (c *ChatsCache) newest() *ChatsSnapshot {
	if len(c.snaps) == 0 {
		return nil
	}
	return c.snaps[len(c.snaps)-1]
}

// startLocked starts a new load on a context detached from ctx. c.mu must be
// held.
func (c *ChatsCache) startLocked(ctx context.Context) *chatsFlight {
	c.started++
	f := &chatsFlight{seq: c.started, done: make(chan struct{}), watchers: make(map[int]ProgressFunc)}
	c.flight = f
	go c.run(context.WithoutCancel(ctx), f)
	return f
}

// run performs the load for f and publishes its snapshot.
func (c *ChatsCache) run(ctx context.Context, f *chatsFlight) {
	ctx, cancel := context.WithTimeout(ctx, chatsLoadTimeout)
	defer cancel()
	result, err := c.load(ctx, f.progress)

	c.mu.Lock()
	if err != nil {
		f.err = err
	} else {
		f.snap = &ChatsSnapshot{
			ID:        randomSnapshotID(),
			Chats:     result.Chats,
			Truncated: result.Truncated,
			loadedAt:  time.Now(),
		}
		c.retainLocked(f.snap)
	}
	c.flight = nil
	c.mu.Unlock()
	close(f.done)
}

// retainLocked appends snap as the newest snapshot and drops those no cursor
// may read any more. c.mu must be held.
func (c *ChatsCache) retainLocked(snap *ChatsSnapshot) {
	c.snaps = slices.DeleteFunc(c.snaps, func(s *ChatsSnapshot) bool {
		return time.Since(s.loadedAt) >= chatsCursorTTL
	})
	c.snaps = append(c.snaps, snap)
	if excess := len(c.snaps) - chatsCursorSnapshots; excess > 0 {
		c.snaps = slices.Delete(c.snaps, 0, excess)
	}
}

// watch subscribes onProgress to f's progress until the returned func is
// called. A nil onProgress hears nothing.
func (f *chatsFlight) watch(onProgress ProgressFunc) func() {
	if onProgress == nil {
		return func() {}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextID
	f.nextID++
	f.watchers[id] = onProgress
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.watchers, id)
	}
}

// progress relays the load's progress to the callers waiting on it.
func (f *chatsFlight) progress(current int, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, onProgress := range f.watchers {
		onProgress(current, message)
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

// randomSnapshotID returns a random non-zero ID, so a cursor from before a
// server restart cannot match a new snapshot.
func randomSnapshotID() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never errors on supported platforms
	//nolint:gosec // G115: intentional full-width uint64→int64 reinterpretation for a random id; every bit pattern is a valid id.
	return int64(binary.LittleEndian.Uint64(b[:])) | 1
}
