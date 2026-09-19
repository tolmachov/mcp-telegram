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
	mu               sync.Mutex
	buckets          map[string]*ipBucket
	rps              rate.Limit
	burst            int
	now              func() time.Time
	trustedProxyHops int
}

func newIPRateLimiter(rps rate.Limit, burst int, trustedProxyHops ...int) *ipRateLimiter {
	hops := 0
	if len(trustedProxyHops) > 0 {
		hops = trustedProxyHops[0]
	}
	return &ipRateLimiter{buckets: map[string]*ipBucket{}, rps: rps, burst: burst, now: time.Now, trustedProxyHops: hops}
}

// wrap returns next guarded by the per-IP limiter.
func (l *ipRateLimiter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r, l.trustedProxyHops)) {
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
// Trust model: a zero trusted-hop count ignores forwarding headers completely.
// With N trusted hops, every X-Forwarded-For element must be a valid IP and the
// selected client is the address immediately to the left of those N proxies;
// malformed or incomplete chains fall back to RemoteAddr.
func clientIP(r *http.Request, trustedProxyHops int) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote := net.ParseIP(host)
	if remote == nil || trustedProxyHops == 0 {
		return host
	}

	entries := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	chain := make([]string, 0, len(entries)+1)
	for _, raw := range entries {
		ip := strings.TrimSpace(raw)
		if net.ParseIP(ip) == nil {
			return host
		}
		chain = append(chain, ip)
	}
	chain = append(chain, remote.String())
	idx := len(chain) - 1 - trustedProxyHops
	if idx < 0 || idx >= len(chain) {
		return host
	}
	return chain[idx]
}
