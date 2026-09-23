// Package keyedlimit is a token-bucket rate limiter with one bucket per key,
// bounded in memory by idle eviction and a size cap.
package keyedlimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// maxKeys caps the bucket map. Keys can be derived from input an attacker
// influences (e.g. forwarding headers), so without a cap a flood of fresh keys
// would grow the map without bound; at the cap the stalest bucket is recycled
// instead.
const maxKeys = 4096

type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// Limiter is a token-bucket limiter keyed by K.
type Limiter[K comparable] struct {
	mu      sync.Mutex
	buckets map[K]*bucket
	rps     rate.Limit
	burst   int
	// refill is how long an untouched bucket takes to fill back up to burst.
	// Past it a bucket is indistinguishable from a fresh one, so dropping it
	// loses nothing.
	refill time.Duration
	now    func() time.Time
}

// New returns a limiter granting each key rps events per second with the
// given burst.
func New[K comparable](rps rate.Limit, burst int) *Limiter[K] {
	return &Limiter[K]{
		buckets: map[K]*bucket{},
		rps:     rps,
		burst:   burst,
		refill:  time.Duration(float64(burst) / float64(rps) * float64(time.Second)),
		now:     time.Now,
	}
}

// Allow reports whether key may perform one event now, consuming a token if so.
func (l *Limiter[K]) Allow(key K) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		l.evictIdleLocked(now)
		if len(l.buckets) >= maxKeys {
			l.evictStalestLocked()
		}
		b = &bucket{limiter: rate.NewLimiter(l.rps, l.burst)}
		l.buckets[key] = b
	}
	b.lastSeen = now
	return b.limiter.Allow()
}

// evictIdleLocked drops buckets that have refilled completely. Called only
// when a new key shows up, so steady-state traffic pays nothing.
func (l *Limiter[K]) evictIdleLocked(now time.Time) {
	for key, b := range l.buckets {
		if now.Sub(b.lastSeen) > l.refill {
			delete(l.buckets, key)
		}
	}
}

// evictStalestLocked recycles the least-recently-seen bucket when the map is
// at capacity. Under a flood of fresh keys this bounds memory while keeping
// the buckets of active callers (they refresh lastSeen constantly).
func (l *Limiter[K]) evictStalestLocked() {
	var stalestKey K
	var stalest time.Time
	found := false
	for key, b := range l.buckets {
		if !found || b.lastSeen.Before(stalest) {
			stalestKey, stalest, found = key, b.lastSeen, true
		}
	}
	if found {
		delete(l.buckets, stalestKey)
	}
}
