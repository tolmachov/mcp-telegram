package authsrv

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
)

// TestSweepOrphanSessions pins the reclamation rule: only sessions whose blob
// age exceeds refreshTokenTTL + sweepMargin are deleted — stale suffixed AND
// stale legacy objects go, anything younger stays.
func TestSweepOrphanSessions(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := sessionstore.NewMemory()

	const staleSID = "0123456789abcdef0123456789abcdef"
	const freshSID = "fedcba9876543210fedcba9876543210"

	// Written at base: unreachable once the clock passes TTL+margin.
	store.Now = func() time.Time { return base }
	require.NoError(t, store.Session(allowedUser, staleSID, nil).StoreSession(ctx, []byte("stale")))
	require.NoError(t, store.Session(allowedUser, "", nil).StoreSession(ctx, []byte("stale-legacy")))

	// Written "now": a live session (gotd re-stores keep active blobs fresh).
	sweepTime := base.Add(defaultRefreshTokenTTL + sweepMargin + time.Hour)
	store.Now = func() time.Time { return sweepTime.Add(-time.Minute) }
	require.NoError(t, store.Session(allowedUser, freshSID, nil).StoreSession(ctx, []byte("fresh")))

	a, _ := newTestServer(t, testConfig(t), store, neverStartLogin)
	a.now = func() time.Time { return sweepTime }
	a.sweepOrphanSessions(ctx)

	for _, tc := range []struct {
		sid  string
		want bool
		desc string
	}{
		{staleSID, false, "stale suffixed session must be reclaimed"},
		{"", false, "stale legacy session must be reclaimed"},
		{freshSID, true, "fresh session must survive"},
	} {
		exists, err := store.Exists(ctx, allowedUser, tc.sid)
		require.NoError(t, err)
		assert.Equal(t, tc.want, exists, tc.desc)
	}
}

// TestSweepRespectsConfiguredTTL pins that a shorter configured
// RefreshTokenTTL tightens the sweep cutoff accordingly.
func TestSweepRespectsConfiguredTTL(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := sessionstore.NewMemory()
	store.Now = func() time.Time { return base }
	const sid = "0123456789abcdef0123456789abcdef"
	require.NoError(t, store.Session(allowedUser, sid, nil).StoreSession(ctx, []byte("s")))

	cfg := testConfig(t)
	cfg.RefreshTokenTTL = 24 * time.Hour
	a, _ := newTestServer(t, cfg, store, neverStartLogin)

	// Just inside the cutoff: kept.
	a.now = func() time.Time { return base.Add(cfg.RefreshTokenTTL + sweepMargin - time.Minute) }
	a.sweepOrphanSessions(ctx)
	exists, err := store.Exists(ctx, allowedUser, sid)
	require.NoError(t, err)
	assert.True(t, exists, "session inside the cutoff must be kept")

	// Just past it: reclaimed.
	a.now = func() time.Time { return base.Add(cfg.RefreshTokenTTL + sweepMargin + time.Minute) }
	a.sweepOrphanSessions(ctx)
	exists, err = store.Exists(ctx, allowedUser, sid)
	require.NoError(t, err)
	assert.False(t, exists, "session past the cutoff must be reclaimed")
}

// TestSweepExpiredTombstones pins that revocation tombstones are reclaimed only
// once older than refreshTokenTTL + sweepMargin — a fresh tombstone survives so
// it can still reject a live refresh token, a stale one is cleaned up.
func TestSweepExpiredTombstones(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := sessionstore.NewMemory()

	const staleSID = "0123456789abcdef0123456789abcdef"
	const freshSID = "fedcba9876543210fedcba9876543210"

	store.Now = func() time.Time { return base }
	require.NoError(t, store.Revoke(ctx, allowedUser, staleSID))

	sweepTime := base.Add(defaultRefreshTokenTTL + sweepMargin + time.Hour)
	store.Now = func() time.Time { return sweepTime.Add(-time.Minute) }
	require.NoError(t, store.Revoke(ctx, allowedUser, freshSID))

	a, _ := newTestServer(t, testConfig(t), store, neverStartLogin)
	a.now = func() time.Time { return sweepTime }
	a.sweepExpiredTombstones(ctx)

	stale, err := store.Revoked(ctx, allowedUser, staleSID)
	require.NoError(t, err)
	assert.False(t, stale, "stale tombstone must be reclaimed")
	fresh, err := store.Revoked(ctx, allowedUser, freshSID)
	require.NoError(t, err)
	assert.True(t, fresh, "fresh tombstone must survive to keep rejecting live tokens")
}
