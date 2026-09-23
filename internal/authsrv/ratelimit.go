package authsrv

import (
	"net"
	"net/http"
	"strings"

	"golang.org/x/time/rate"

	"github.com/tolmachov/mcp-telegram/internal/keyedlimit"
)

// Per-IP budget for auth endpoints. Generous for interactive use, tight
// enough to blunt token-grinding: sealed blobs are 256-bit AEAD, so this is
// defense in depth, not the security boundary. Cloud Armor is the answer to
// volumetric abuse.
const (
	rateLimitPerSecond = 5
	rateLimitBurst     = 20
)

// ipRateLimiter is a token-bucket limiter keyed by client IP.
type ipRateLimiter struct {
	limiter          *keyedlimit.Limiter[string]
	trustedProxyHops int
}

func newIPRateLimiter(rps rate.Limit, burst, trustedProxyHops int) *ipRateLimiter {
	return &ipRateLimiter{limiter: keyedlimit.New[string](rps, burst), trustedProxyHops: trustedProxyHops}
}

// wrap returns next guarded by the per-IP limiter.
func (l *ipRateLimiter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.limiter.Allow(clientIP(r, l.trustedProxyHops)) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
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
