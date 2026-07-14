package authsrv

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Per-IP budget for auth endpoints. Generous for interactive use, tight
// enough to blunt token-grinding: sealed blobs are 256-bit AEAD, so this is
// defense in depth, not the security boundary. Cloud Armor is the answer to
// volumetric abuse.
const (
	rateLimitPerSecond = 5
	rateLimitBurst     = 20
	// rateLimiterMaxIdle is how long an idle IP keeps its bucket.
	rateLimiterMaxIdle = 10 * time.Minute
	// rateLimiterMaxBuckets caps the bucket map. The client-IP key is
	// derived from headers an attacker can influence, so without a cap a
	// spoofed-IP flood would grow the map without bound; at the cap the
	// stalest bucket is recycled instead.
	rateLimiterMaxBuckets = 4096
)

type ipBucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// ipRateLimiter is a token-bucket limiter keyed by client IP.
type ipRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*ipBucket
	rps     rate.Limit
	burst   int
	now     func() time.Time
}

func newIPRateLimiter(rps rate.Limit, burst int) *ipRateLimiter {
	return &ipRateLimiter{buckets: map[string]*ipBucket{}, rps: rps, burst: burst, now: time.Now}
}

// wrap returns next guarded by the per-IP limiter.
func (l *ipRateLimiter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (l *ipRateLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[ip]
	if !ok {
		l.evictIdleLocked(now)
		if len(l.buckets) >= rateLimiterMaxBuckets {
			l.evictStalestLocked()
		}
		b = &ipBucket{limiter: rate.NewLimiter(l.rps, l.burst)}
		l.buckets[ip] = b
	}
	b.lastSeen = now
	return b.limiter.Allow()
}

// evictIdleLocked drops buckets not seen within rateLimiterMaxIdle. Called
// only when a new IP shows up, so steady-state traffic pays nothing.
func (l *ipRateLimiter) evictIdleLocked(now time.Time) {
	for ip, b := range l.buckets {
		if now.Sub(b.lastSeen) > rateLimiterMaxIdle {
			delete(l.buckets, ip)
		}
	}
}

// evictStalestLocked recycles the least-recently-seen bucket when the map is
// at capacity. Under a spoofed-IP flood this bounds memory while keeping
// active well-behaved clients' buckets (they refresh lastSeen constantly).
func (l *ipRateLimiter) evictStalestLocked() {
	var stalestKey string
	var stalest time.Time
	for ip, b := range l.buckets {
		if stalestKey == "" || b.lastSeen.Before(stalest) {
			stalestKey, stalest = ip, b.lastSeen
		}
	}
	if stalestKey != "" {
		delete(l.buckets, stalestKey)
	}
}

// clientIP extracts the caller's IP for rate-limit bucketing.
//
// Trust model: proxies APPEND to X-Forwarded-For, so on Cloud Run the
// rightmost entry is the connection IP recorded by the trusted Google front
// end, while everything to its left (and the whole header when no proxy is
// involved) is attacker-supplied. We therefore take the LAST entry and fall
// back to RemoteAddr when the header is absent. When the server runs
// without a trusted proxy, a client can still choose its own bucket by
// setting the header — the map cap plus per-bucket limits keep that
// spoofing bounded, and the limiter is defense in depth, not the security
// boundary.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		entries := strings.Split(xff, ",")
		if ip := strings.TrimSpace(entries[len(entries)-1]); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
