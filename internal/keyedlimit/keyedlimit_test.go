package keyedlimit

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLimiterBurstPerKey(t *testing.T) {
	l := New[string](1, 3)
	for i := range 3 {
		assert.Truef(t, l.Allow("a"), "event %d within burst", i)
	}
	assert.False(t, l.Allow("a"))
	assert.True(t, l.Allow("b"), "a different key gets its own bucket")
}

func TestLimiterEvictsRefilledBuckets(t *testing.T) {
	l := New[int](1, 2)
	base := time.Now()
	l.now = func() time.Time { return base }

	require.True(t, l.Allow(1))
	require.True(t, l.Allow(2))

	// Key 1 has been idle past its refill window; key 2 was just seen. The
	// arrival of a new key sweeps only the refilled bucket.
	l.now = func() time.Time { return base.Add(l.refill + time.Millisecond) }
	require.True(t, l.Allow(2))
	l.now = func() time.Time { return base.Add(l.refill + 2*time.Millisecond) }
	require.True(t, l.Allow(3))
	assert.NotContains(t, l.buckets, 1)
	assert.Contains(t, l.buckets, 2)
}

func TestLimiterRefillWindowMatchesBurst(t *testing.T) {
	assert.Equal(t, 4*time.Second, New[int](5, 20).refill)
}

func TestLimiterCapRecyclesStalest(t *testing.T) {
	l := New[int](1, 1)
	base := time.Now()

	// Fill to the cap with strictly increasing lastSeen, all within the
	// refill window so idle eviction does not interfere.
	for i := range maxKeys {
		l.now = func() time.Time { return base.Add(time.Duration(i) * time.Microsecond) }
		require.True(t, l.Allow(i))
	}
	require.Len(t, l.buckets, maxKeys)

	// The next new key must not grow the map past the cap, and the stalest
	// bucket (0) is the one recycled.
	l.now = func() time.Time { return base.Add(time.Duration(maxKeys) * time.Microsecond) }
	require.True(t, l.Allow(-1))
	assert.Len(t, l.buckets, maxKeys)
	assert.NotContains(t, l.buckets, 0)
	assert.Contains(t, l.buckets, -1)
}
