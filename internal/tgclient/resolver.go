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
	// peerResolveTimeout bounds one shared resolve, which runs on the
	// resolver's lifetime rather than on any of the callers waiting on it: up
	// to three probes, each of which may sit out a flood wait.
	peerResolveTimeout = 5 * time.Minute
)

type peerCacheEntry struct {
	peer      Peer
	expiresAt time.Time
}

// Resolver is the one peer resolver of an assembly: every tool and data path
// that turns a chat ID into a peer goes through it. It caches resolved peers
// in a bounded, expiring map, collapses concurrent cold resolves of the same
// ID into one Telegram probe, and owns the stale-access-hash retry (WithPeer
// and its variants).
type Resolver struct {
	// life is the lifetime of the resolver's owner; probes run on it.
	life   context.Context
	client *tg.Client

	mu   sync.RWMutex
	byID map[int64]peerCacheEntry
	load singleflight.Group
	now  func() time.Time
}

// NewResolver creates a resolver over client. life is the lifetime of the
// resolver's owner: every probe runs on it, so ending it cancels a probe in
// progress and fails whoever waits on it.
func NewResolver(life context.Context, client *tg.Client) *Resolver {
	return &Resolver{
		life:   life,
		client: client,
		byID:   make(map[int64]peerCacheEntry),
		now:    time.Now,
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
	entry, ok := r.byID[id]
	r.mu.RUnlock()
	if !ok || !r.now().Before(entry.expiresAt) {
		return Peer{}, false
	}
	return entry.peer, true
}

// store caches peer under id. Only a full cache is swept: expired entries go
// first and, if none had expired, one arbitrary entry makes room.
func (r *Resolver) store(id int64, peer Peer) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[id]; !ok && len(r.byID) >= peerCacheMaxEntries {
		for victim, entry := range r.byID {
			if !now.Before(entry.expiresAt) {
				delete(r.byID, victim)
			}
		}
		if len(r.byID) >= peerCacheMaxEntries {
			for victim := range r.byID {
				delete(r.byID, victim)
				break
			}
		}
	}
	r.byID[id] = peerCacheEntry{peer: peer, expiresAt: now.Add(peerCacheTTL)}
}

// Invalidate drops the cached peers for ids.
func (r *Resolver) Invalidate(ids ...int64) {
	r.mu.Lock()
	for _, id := range ids {
		delete(r.byID, id)
	}
	r.mu.Unlock()
}

// WithPeer resolves id and runs op with its peer. When op fails with a stale
// access hash (ShouldRefreshPeer) it drops the cached peer and runs op exactly
// once more with a freshly resolved one.
func WithPeer[T any](ctx context.Context, r *Resolver, id int64, op func(Peer) (T, error)) (T, error) {
	return withPeers(ctx, r, []int64{id}, nil, func(peers []Peer) (T, error) { return op(peers[0]) })
}

// WithPeers is WithPeer for several chat IDs: op gets their peers in the same
// order, and a stale access hash re-resolves all of them.
func WithPeers[T any](ctx context.Context, r *Resolver, ids []int64, op func([]Peer) (T, error)) (T, error) {
	return withPeers(ctx, r, ids, nil, op)
}

// WithPeerKeepingPartial is WithPeer for an op that can fail after making
// progress: when partial reports that a failed attempt's result holds work a
// retry would throw away, that result is returned with its error instead of
// being retried.
func WithPeerKeepingPartial[T any](ctx context.Context, r *Resolver, id int64, partial func(T) bool, op func(Peer) (T, error)) (T, error) {
	return withPeers(ctx, r, []int64{id}, partial, func(peers []Peer) (T, error) { return op(peers[0]) })
}

// WithPeersFrom is WithPeers for peers that do not all come from chat IDs,
// such as a mix of @usernames and IDs: resolve produces them, and a stale
// access hash drops every peer op was given from the cache and runs resolve
// and op once more.
func WithPeersFrom[T any](r *Resolver, resolve func() ([]Peer, error), op func([]Peer) (T, error)) (T, error) {
	return retryStale(r, resolve, nil, op)
}

// withPeers resolves ids through r for retryStale; partial may be nil.
func withPeers[T any](ctx context.Context, r *Resolver, ids []int64, partial func(T) bool, op func([]Peer) (T, error)) (T, error) {
	return retryStale(r, func() ([]Peer, error) { return r.resolveAll(ctx, ids) }, partial, op)
}

// retryStale is the one stale-hash retry behind WithPeer and its variants:
// when op fails with a stale access hash (and partial, if set, finds nothing
// worth keeping in its result), the peers it was given leave the cache and
// resolve and op run exactly once more.
func retryStale[T any](r *Resolver, resolve func() ([]Peer, error), partial func(T) bool, op func([]Peer) (T, error)) (T, error) {
	var zero T
	peers, err := resolve()
	if err != nil {
		return zero, err
	}
	result, err := op(peers)
	if err == nil || !ShouldRefreshPeer(err) || (partial != nil && partial(result)) {
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
