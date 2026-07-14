package authsrv

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIPRateLimiterBurstThen429(t *testing.T) {
	l := newIPRateLimiter(1, 3)
	handler := l.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	do := func(remoteAddr, xff string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/token", nil)
		req.RemoteAddr = remoteAddr
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	for i := range 3 {
		assert.Equalf(t, http.StatusOK, do("10.0.0.1:1234", "").Code, "request %d within burst", i)
	}
	rec := do("10.0.0.1:1234", "")
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))

	// A different client IP gets its own bucket.
	assert.Equal(t, http.StatusOK, do("10.0.0.2:1234", "").Code)
}

func TestIPRateLimiterTrustsAppendedXFFEntry(t *testing.T) {
	l := newIPRateLimiter(1, 1)

	// The rightmost entry is what the trusted proxy appended; everything the
	// client sent itself must not create fresh buckets.
	assert.True(t, l.allow(clientIPFromXFF("spoof-1, 198.51.100.7")))
	assert.False(t, l.allow(clientIPFromXFF("spoof-2, 198.51.100.7")),
		"rotating the client-supplied XFF prefix must not evade the limit")
	assert.True(t, l.allow(clientIPFromXFF("spoof-3, 198.51.100.8")))
}

func clientIPFromXFF(xff string) string {
	req := httptest.NewRequest(http.MethodPost, "/token", nil)
	req.RemoteAddr = "127.0.0.1:9"
	req.Header.Set("X-Forwarded-For", xff)
	return clientIP(req)
}

func TestClientIPFallsBackToRemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/token", nil)
	req.RemoteAddr = "192.0.2.9:4433"
	assert.Equal(t, "192.0.2.9", clientIP(req))
}

func TestIPRateLimiterEvictsIdleBuckets(t *testing.T) {
	l := newIPRateLimiter(1, 1)
	base := time.Now()
	l.now = func() time.Time { return base }

	require.True(t, l.allow("10.0.0.1"))
	require.Len(t, l.buckets, 1)

	// Past the idle window, the arrival of a new IP sweeps the stale bucket.
	l.now = func() time.Time { return base.Add(rateLimiterMaxIdle + time.Minute) }
	require.True(t, l.allow("10.0.0.2"))
	assert.NotContains(t, l.buckets, "10.0.0.1")
}

func TestIPRateLimiterCapRecyclesStalest(t *testing.T) {
	l := newIPRateLimiter(1, 1)
	base := time.Now()

	// Fill to the cap with strictly increasing lastSeen.
	for i := range rateLimiterMaxBuckets {
		l.now = func() time.Time { return base.Add(time.Duration(i) * time.Millisecond) }
		require.True(t, l.allow(fmt.Sprintf("ip-%d", i)))
	}
	require.Len(t, l.buckets, rateLimiterMaxBuckets)

	// The next new key must not grow the map past the cap, and the stalest
	// bucket (ip-0) is the one recycled.
	l.now = func() time.Time { return base.Add(time.Second) }
	require.True(t, l.allow("ip-new"))
	assert.Len(t, l.buckets, rateLimiterMaxBuckets)
	assert.NotContains(t, l.buckets, "ip-0")
	assert.Contains(t, l.buckets, "ip-new")
}
