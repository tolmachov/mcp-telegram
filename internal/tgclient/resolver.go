package tgclient

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"golang.org/x/sync/singleflight"
	"golang.org/x/time/rate"
)

const (
	peerCacheMaxEntries = 4096
	peerCacheTTL        = time.Hour
	// peerResolveTimeout bounds one shared resolve, which runs detached from
	// the callers waiting on it: up to three probes, each of which may sit
	// out a flood wait.
	peerResolveTimeout = 5 * time.Minute
)

type peerCacheEntry struct {
	peer      Peer
	expiresAt time.Time
}

// Resolver is the one peer resolver of an assembly: every tool and data path
// that turns a chat ID into a peer goes through it. It caches resolved peers
// in a bounded, expiring map, collapses concurrent cold resolves of the same
// ID into one Telegram probe, and owns both the stale-access-hash retry
// (WithPeer, WithPeers) and the rate limiter that paces message fetching.
type Resolver struct {
	client  *tg.Client
	limiter *rate.Limiter

	mu   sync.RWMutex
	byID map[int64]peerCacheEntry
	load singleflight.Group
	now  func() time.Time
}

// NewResolver creates a resolver over client whose limiter admits rps
// requests per second (the default lives on --tg-rate-limit-rps).
func NewResolver(client *tg.Client, rps int) *Resolver {
	return &Resolver{
		client:  client,
		limiter: rate.NewLimiter(rate.Limit(rps), 1),
		byID:    make(map[int64]peerCacheEntry),
		now:     time.Now,
	}
}

// Client returns the Telegram client the resolver resolves through.
func (r *Resolver) Client() *tg.Client { return r.client }

// Wait blocks until the rate limiter admits one Telegram request.
func (r *Resolver) Wait(ctx context.Context) error {
	if err := r.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for Telegram rate limit: %w", err)
	}
	return nil
}

// Resolve returns the peer for id, from the cache when it holds a live entry.
// Failures are never cached, so a transient error (e.g. a flood wait) is
// retried on the next call. Every failure is a *PeerError.
//
// Concurrent cold resolves of id share one probe. It belongs to none of them:
// it runs on a context detached from the caller that started it (bounded by
// peerResolveTimeout), and each caller waits on its own ctx, so one caller
// giving up neither fails the others nor wastes the probe.
func (r *Resolver) Resolve(ctx context.Context, id int64) (Peer, error) {
	if peer, ok := r.cached(id); ok {
		return peer, nil
	}
	flight := r.load.DoChan(strconv.FormatInt(id, 10), func() (any, error) {
		if peer, ok := r.cached(id); ok {
			return peer, nil
		}
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), peerResolveTimeout)
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
		return Peer{}, &PeerError{ID: id, Err: ctx.Err()}
	case outcome := <-flight:
		if outcome.Err != nil {
			return Peer{}, outcome.Err //nolint:wrapcheck // resolvePeer already returns a *PeerError naming the chat.
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

// WithPeers resolves ids and runs op with their peers, in the same order. When
// op fails with a stale access hash (ShouldRefreshPeer) it invalidates ids and
// related — peers op used without resolving them here — and runs op exactly
// once more with freshly resolved peers. keep, when non-nil, reports whether a
// failed attempt's result holds progress a retry would throw away; such a
// result is returned with its error instead of being retried.
func WithPeers[T any](ctx context.Context, r *Resolver, ids, related []int64, keep func(T) bool, op func([]Peer) (T, error)) (T, error) {
	var zero T
	peers, err := r.resolveAll(ctx, ids)
	if err != nil {
		return zero, err
	}
	result, err := op(peers)
	if err == nil || !ShouldRefreshPeer(err) || (keep != nil && keep(result)) {
		return result, err
	}
	r.Invalidate(ids...)
	r.Invalidate(related...)
	if peers, err = r.resolveAll(ctx, ids); err != nil {
		return zero, err
	}
	return op(peers)
}

// WithPeer is WithPeers for a single chat ID.
func WithPeer[T any](ctx context.Context, r *Resolver, id int64, related []int64, keep func(T) bool, op func(Peer) (T, error)) (T, error) {
	return WithPeers(ctx, r, []int64{id}, related, keep, func(peers []Peer) (T, error) {
		return op(peers[0])
	})
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
