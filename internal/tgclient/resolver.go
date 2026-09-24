package tgclient

import (
	"container/list"
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
	// peerResolveTimeout bounds one shared resolve, which runs on the
	// resolver's lifetime rather than on any of the callers waiting on it: up
	// to three probes, each of which may sit out a flood wait.
	peerResolveTimeout = 5 * time.Minute
)

type peerCacheEntry struct {
	id        int64
	peer      Peer
	expiresAt time.Time
}

// Resolver is the one peer resolver of an assembly: every tool and data path
// that turns a chat ID into a peer goes through it. It caches resolved peers
// in a bounded, expiring map, collapses concurrent cold resolves of the same
// ID into one Telegram probe, and owns the stale-access-hash retry (WithPeer
// and its variants). Entities Telegram returns alongside other answers — a
// username resolution, a dialog listing, a contact search — feed the cache
// too (Remember), so an ID the model got from them resolves without a probe.
type Resolver struct {
	// life is the lifetime of the resolver's owner; probes run on it.
	life   context.Context
	client *tg.Client

	mu   sync.RWMutex
	byID map[int64]*list.Element
	// order holds the cached entries (*peerCacheEntry) oldest store first.
	// Every entry lives peerCacheTTL, so this is also expiry order: the
	// front is the first to expire, and the one a full cache drops.
	order *list.List
	load  singleflight.Group
}

// NewResolver creates a resolver over client. life is the lifetime of the
// resolver's owner: every probe runs on it, so ending it cancels a probe in
// progress and fails whoever waits on it with why it ended (its cause).
func NewResolver(life context.Context, client *tg.Client) *Resolver {
	return &Resolver{
		life:   life,
		client: client,
		byID:   make(map[int64]*list.Element),
		order:  list.New(),
	}
}

// Client returns the Telegram client the resolver resolves through.
func (r *Resolver) Client() *tg.Client { return r.client }

// Resolve returns the peer for id, from the cache when it holds a live entry.
// Failures are never cached, so a transient error (e.g. a flood wait) is
// retried on the next call. Every failure names the chat, and one that is down
// to the ID itself is ErrUnresolvablePeer (see resolvePeer).
//
// Concurrent cold resolves of id share one probe. It belongs to none of them:
// it runs on the resolver's lifetime (see NewResolver) bounded by
// peerResolveTimeout, never on a caller's context or its values, and each
// caller waits on its own ctx, so one caller giving up neither fails the
// others nor wastes the probe.
func (r *Resolver) Resolve(ctx context.Context, id int64) (Peer, error) {
	if peer, ok := r.cached(id); ok {
		return peer, nil
	}
	flight := r.load.DoChan(strconv.FormatInt(id, 10), func() (any, error) {
		if peer, ok := r.cached(id); ok {
			return peer, nil
		}
		probeCtx, cancel := context.WithTimeout(r.life, peerResolveTimeout)
		defer cancel()
		peer, err := resolvePeer(probeCtx, r.client, id)
		if err != nil {
			// A probe the owner's end cut short fails with why it ended
			// (e.g. the client stopping), not a bare cancellation.
			if cause := context.Cause(r.life); cause != nil {
				return nil, fmt.Errorf("resolving chat %d: %w", id, cause)
			}
			return nil, err
		}
		r.store(id, peer)
		return peer, nil
	})
	select {
	case <-ctx.Done():
		return Peer{}, fmt.Errorf("resolving chat %d: %w", id, ctx.Err())
	case outcome := <-flight:
		if outcome.Err != nil {
			return Peer{}, outcome.Err //nolint:wrapcheck // resolvePeer's errors already name the chat.
		}
		return outcome.Val.(Peer), nil
	}
}

func (r *Resolver) cached(id int64) (Peer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	el, ok := r.byID[id]
	if !ok {
		return Peer{}, false
	}
	entry := el.Value.(*peerCacheEntry)
	if !time.Now().Before(entry.expiresAt) {
		return Peer{}, false
	}
	return entry.peer, true
}

// store caches peer under id for peerCacheTTL. A full cache drops its oldest
// entry, which is the first to expire: an expired one when there is any.
func (r *Resolver) store(id int64, peer Peer) {
	entry := &peerCacheEntry{id: id, peer: peer, expiresAt: time.Now().Add(peerCacheTTL)}
	r.mu.Lock()
	defer r.mu.Unlock()
	if el, ok := r.byID[id]; ok {
		el.Value = entry
		r.order.MoveToBack(el)
		return
	}
	if r.order.Len() >= peerCacheMaxEntries {
		oldest := r.order.Front()
		delete(r.byID, oldest.Value.(*peerCacheEntry).id)
		r.order.Remove(oldest)
	}
	r.byID[id] = r.order.PushBack(entry)
}

// Remember caches the peers of the users and chats Telegram returned with an
// answer. An entity no InputPeer can be built from is skipped: one without
// its access hash (see PeerFromEntity), a min entity, whose access hash is
// valid only alongside the message it came with, and a forbidden chat.
func (r *Resolver) Remember(users []tg.UserClass, chats []tg.ChatClass) {
	for _, u := range users {
		if user, ok := u.(*tg.User); ok && !user.Min {
			r.remember(PeerFromEntity(user))
		}
	}
	for _, c := range chats {
		switch chat := c.(type) {
		case *tg.Chat:
			r.remember(PeerFromEntity(chat))
		case *tg.Channel:
			if !chat.Min {
				r.remember(PeerFromEntity(chat))
			}
		}
	}
}

// remember caches peer unless building it failed.
func (r *Resolver) remember(peer Peer, err error) {
	if err == nil {
		r.store(peer.ID(), peer)
	}
}

// Invalidate drops the cached peers for ids.
func (r *Resolver) Invalidate(ids ...int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		if el, ok := r.byID[id]; ok {
			delete(r.byID, id)
			r.order.Remove(el)
		}
	}
}

// WithPeer resolves id and runs op with its peer. When op fails with a stale
// access hash (shouldRefreshPeer) it drops the cached peer and runs op exactly
// once more with a freshly resolved one. An op that pages keeps each page to
// its own WithPeer, so a stale hash re-resolves without losing the pages
// already fetched.
func WithPeer[T any](ctx context.Context, r *Resolver, id int64, op func(Peer) (T, error)) (T, error) {
	return WithPeers(ctx, r, []int64{id}, func(peers []Peer) (T, error) { return op(peers[0]) })
}

// WithPeers is WithPeer for several chat IDs: op gets their peers in the same
// order, and a stale access hash re-resolves all of them.
func WithPeers[T any](ctx context.Context, r *Resolver, ids []int64, op func([]Peer) (T, error)) (T, error) {
	return WithPeersFrom(r, func() ([]Peer, error) { return r.resolveAll(ctx, ids) }, op)
}

// WithPeersFrom is WithPeers for peers that do not all come from chat IDs,
// such as a mix of @usernames and IDs: resolve produces them, and a stale
// access hash drops every peer op was given from the cache and runs resolve
// and op once more. It is the one stale-hash retry behind WithPeer and its
// variants.
func WithPeersFrom[T any](r *Resolver, resolve func() ([]Peer, error), op func([]Peer) (T, error)) (T, error) {
	var zero T
	peers, err := resolve()
	if err != nil {
		return zero, err
	}
	result, err := op(peers)
	if err == nil || !shouldRefreshPeer(err) {
		return result, err
	}
	for _, peer := range peers {
		r.Invalidate(peer.ID())
	}
	if peers, err = resolve(); err != nil {
		return zero, err
	}
	return op(peers)
}

func (r *Resolver) resolveAll(ctx context.Context, ids []int64) ([]Peer, error) {
	peers := make([]Peer, len(ids))
	for i, id := range ids {
		peer, err := r.Resolve(ctx, id)
		if err != nil {
			return nil, err
		}
		peers[i] = peer
	}
	return peers, nil
}
