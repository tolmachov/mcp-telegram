// Package keyedlimit is a token-bucket rate limiter with one bucket per key,
// bounded in memory by a size cap.
package keyedlimit

import (
	"container/list"
	"fmt"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// maxKeys caps the bucket map. Keys can be derived from input an attacker
// influences (e.g. forwarding headers), so without a cap a flood of fresh keys
// would grow the map without bound; at the cap the stalest bucket is recycled
// instead.
const maxKeys = 4096

type bucket[K comparable] struct {
	key      K
	limiter  *rate.Limiter
	lastSeen time.Time
}

// Limiter is a token-bucket limiter keyed by K.
type Limiter[K comparable] struct {
	mu      sync.Mutex
	buckets map[K]*list.Element
	// bySeen holds the *bucket[K] values ordered by lastSeen, least recent at
	// the front: every Allow moves its bucket to the back, so eviction takes
	// from the front without scanning the map.
	bySeen *list.List
	rps    rate.Limit
	burst  int
	// refill is how long an untouched bucket takes to fill back up to burst.
	// Past it a bucket is indistinguishable from a fresh one, so dropping it
	// loses nothing.
	refill time.Duration
	// now is the one clock behind both the buckets' tokens and lastSeen.
	now func() time.Time
}

// New returns a limiter granting each key rps events per second with the
// given burst. Both must be positive: a limiter that can never grant an event
// is a configuration bug, so New panics on one.
func New[K comparable](rps rate.Limit, burst int) *Limiter[K] {
	if rps <= 0 || burst <= 0 {
		panic(fmt.Sprintf("keyedlimit: rps (%v) and burst (%d) must be positive", rps, burst))
	}
	return &Limiter[K]{
		buckets: map[K]*list.Element{},
		bySeen:  list.New(),
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
	var b *bucket[K]
	if el, ok := l.buckets[key]; ok {
		l.bySeen.MoveToBack(el)
		b = bucketOf[K](el)
	} else {
		if len(l.buckets) >= maxKeys {
			l.evictLocked(now)
		}
		b = &bucket[K]{key: key, limiter: rate.NewLimiter(l.rps, l.burst)}
		l.buckets[key] = l.bySeen.PushBack(b)
	}
	b.lastSeen = now
	return b.limiter.AllowN(now, 1)
}

// bucketOf returns the bucket an element of bySeen holds; the list stores
// nothing else.
func bucketOf[K comparable](el *list.Element) *bucket[K] {
	return el.Value.(*bucket[K])
}

// evictLocked makes room for a new key in a full map: it drops every bucket
// that has refilled completely and, if none has, the least-recently-seen one.
// Buckets leave from the front of bySeen, where the least recently seen sit,
// so each removal is O(1) and the refilled ones are exactly a prefix. Called
// only when a new key meets a full map, so traffic below the cap pays
// nothing. Under a flood of fresh keys the cap bounds memory while keeping the
// buckets of active callers (they refresh lastSeen constantly).
func (l *Limiter[K]) evictLocked(now time.Time) {
	for front := l.bySeen.Front(); front != nil; front = l.bySeen.Front() {
		if now.Sub(bucketOf[K](front).lastSeen) <= l.refill {
			break
		}
		l.removeLocked(front)
	}
	if len(l.buckets) >= maxKeys {
		l.removeLocked(l.bySeen.Front())
	}
}

func (l *Limiter[K]) removeLocked(el *list.Element) {
	delete(l.buckets, bucketOf[K](el).key)
	l.bySeen.Remove(el)
}
