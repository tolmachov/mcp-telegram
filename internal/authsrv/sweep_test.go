package authsrv

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore/sessionstoretest"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// TestSweepOrphanSessions pins the reclamation rule: only sessions whose blob
// age exceeds refreshTokenTTL + sweepMargin are deleted; anything younger
// stays.
func TestSweepOrphanSessions(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeTime := base
	store := sessionstoretest.NewWithClock(t, func() time.Time { return writeTime })

	const staleSID = "0123456789abcdef0123456789abcdef"
	const freshSID = "fedcba9876543210fedcba9876543210"

	// Written at base: unreachable once the clock passes TTL+margin.
	require.NoError(t, store.Session(allowedUser, staleSID, sessionKey(t)).StoreSession(ctx, []byte("stale")))

	// Written "now": a live session (gotd re-stores keep active blobs fresh).
	sweepTime := base.Add(refreshTokenTTL + sweepMargin + time.Hour)
	writeTime = sweepTime.Add(-time.Minute)
	require.NoError(t, store.Session(allowedUser, freshSID, sessionKey(t)).StoreSession(ctx, []byte("fresh")))

	a, _ := newTestServer(t, testConfig(t), store, neverStartLogin)
	a.now = func() time.Time { return sweepTime }
	a.runSweep(ctx)

	for _, tc := range []struct {
		sid  string
		want bool
		desc string
	}{
		{staleSID, false, "stale suffixed session must be reclaimed"},
		{freshSID, true, "fresh session must survive"},
	} {
		exists, err := store.Exists(ctx, allowedUser, tc.sid)
		require.NoError(t, err)
		assert.Equal(t, tc.want, exists, tc.desc)
	}
}

// TestSweepSessionCutoffBoundary pins the session cutoff on both sides of
// refreshTokenTTL + sweepMargin.
func TestSweepSessionCutoffBoundary(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := sessionstoretest.NewWithClock(t, func() time.Time { return base })
	const sid = "0123456789abcdef0123456789abcdef"
	require.NoError(t, store.Session(allowedUser, sid, sessionKey(t)).StoreSession(ctx, []byte("s")))

	a, _ := newTestServer(t, testConfig(t), store, neverStartLogin)

	// Just inside the cutoff: kept.
	a.now = func() time.Time { return base.Add(refreshTokenTTL + sweepMargin - time.Minute) }
	a.runSweep(ctx)
	exists, err := store.Exists(ctx, allowedUser, sid)
	require.NoError(t, err)
	assert.True(t, exists, "session inside the cutoff must be kept")

	// Just past it: reclaimed.
	a.now = func() time.Time { return base.Add(refreshTokenTTL + sweepMargin + time.Minute) }
	a.runSweep(ctx)
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
	writeTime := base
	store := sessionstoretest.NewWithClock(t, func() time.Time { return writeTime })

	const staleSID = "0123456789abcdef0123456789abcdef"
	const freshSID = "fedcba9876543210fedcba9876543210"

	require.NoError(t, store.Revoke(ctx, allowedUser, staleSID))

	sweepTime := base.Add(refreshTokenTTL + sweepMargin + time.Hour)
	writeTime = sweepTime.Add(-time.Minute)
	require.NoError(t, store.Revoke(ctx, allowedUser, freshSID))

	a, _ := newTestServer(t, testConfig(t), store, neverStartLogin)
	a.now = func() time.Time { return sweepTime }
	a.runSweep(ctx)

	stale, err := store.Revoked(ctx, allowedUser, staleSID)
	require.NoError(t, err)
	assert.False(t, stale, "stale tombstone must be reclaimed")
	fresh, err := store.Revoked(ctx, allowedUser, freshSID)
	require.NoError(t, err)
	assert.True(t, fresh, "fresh tombstone must survive to keep rejecting live tokens")
}

// TestSweepTombstoneCutoffBoundary pins the tombstone cutoff on BOTH
// sides of refreshTokenTTL + sweepMargin. The two-sided boundary matters more
// here than for session blobs: a tombstone reclaimed early (say after an hour)
// would re-open the exact resurrection hole tombstones exist to close — a warm
// client re-stores the blob, the tombstone is gone, and a revoked refresh token
// works again. TestSweepExpiredTombstones alone cannot catch that mutation (its
// fresh tombstone is only a minute old at sweep time).
func TestSweepTombstoneCutoffBoundary(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := sessionstoretest.NewWithClock(t, func() time.Time { return base })
	const sid = "0123456789abcdef0123456789abcdef"
	require.NoError(t, store.Revoke(ctx, allowedUser, sid))

	a, _ := newTestServer(t, testConfig(t), store, neverStartLogin)

	// Just inside the cutoff: the tombstone must survive — a refresh token from
	// the revoked grant could still be presented until LoginAt+TTL, and the
	// margin absorbs clock skew on top.
	a.now = func() time.Time { return base.Add(refreshTokenTTL + sweepMargin - time.Minute) }
	a.runSweep(ctx)
	revoked, err := store.Revoked(ctx, allowedUser, sid)
	require.NoError(t, err)
	assert.True(t, revoked, "tombstone inside the cutoff must survive to keep rejecting live tokens")

	// Just past it: reclaimed.
	a.now = func() time.Time { return base.Add(refreshTokenTTL + sweepMargin + time.Minute) }
	a.runSweep(ctx)
	revoked, err = store.Revoked(ctx, allowedUser, sid)
	require.NoError(t, err)
	assert.False(t, revoked, "tombstone past the cutoff must be reclaimed")
}

// sessionKey returns a fresh per-session key of the length the store requires.
func sessionKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	return key
}

// panicListStore panics from List, standing in for a store backend that faults
// (or hits a bug) mid-sweep.
type panicListStore struct {
	sessionstore.Store
}

func (panicListStore) List(context.Context) ([]sessionstore.SessionRef, error) {
	panic("simulated backend panic during List")
}

// TestSweepIsolatesPanickingPart pins that a store panicking in one sweep part
// on every run neither unwinds the sweeper goroutine nor keeps the other parts
// from reclaiming what they own.
func TestSweepIsolatesPanickingPart(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := panicListStore{Store: sessionstoretest.NewWithClock(t, func() time.Time { return base })}
	const sid = "0123456789abcdef0123456789abcdef"
	const family = "fedcba9876543210fedcba9876543210"
	require.NoError(t, store.Revoke(ctx, allowedUser, sid))
	created, err := store.RedeemCode(ctx, family, base.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, created)

	a, _ := newTestServer(t, testConfig(t), store, neverStartLogin)
	a.now = func() time.Time { return base.Add(refreshTokenTTL + sweepMargin + time.Minute) }
	assert.NotPanics(t, func() { a.runSweep(ctx) })

	revoked, err := store.Revoked(ctx, allowedUser, sid)
	require.NoError(t, err)
	assert.False(t, revoked, "the tombstone sweep must run despite the session sweep panicking")
	// Redeeming the family again succeeds only once its record is gone.
	created, err = store.RedeemCode(ctx, family, base.Add(time.Hour))
	require.NoError(t, err)
	assert.True(t, created, "the grant sweep must run despite the session sweep panicking")
}

// sweepResultStore answers SweepAuthState with a fixed result.
type sweepResultStore struct {
	sessionstore.Store
	undecodable []string
	err         error
}

func (s sweepResultStore) SweepAuthState(context.Context, time.Time) ([]string, error) {
	return s.undecodable, s.err
}

// newLoggingServer returns an AuthServer over store whose log goes to the
// returned buffer.
func newLoggingServer(t *testing.T, store sessionstore.Store) (*AuthServer, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	a, err := New(testConfig(t), slog.New(slog.NewTextHandler(&logs, nil)), store, neverStartLogin, noInvalidate)
	require.NoError(t, err)
	t.Cleanup(a.Close)
	return a, &logs
}

// TestGrantSweepReportsUndecodableRecords pins that every undecodable grant
// record the store deleted is logged as a warning, alongside the failures of
// the same sweep.
func TestGrantSweepReportsUndecodableRecords(t *testing.T) {
	const family = "fedcba9876543210fedcba9876543210"
	a, logs := newLoggingServer(t, sweepResultStore{
		Store: sessionstoretest.New(t), undecodable: []string{family}, err: errors.New("boom"),
	})
	a.runSweep(t.Context())
	assert.Contains(t, logs.String(),
		`level=WARN msg="oauth state sweep: deleted an undecodable grant record" family=`+family)
	assert.Contains(t, logs.String(), `level=ERROR msg="oauth state sweep failed" err=boom`)
}

// cancelOnDeleteStore stands in for shutdown arriving mid-sweep: its Delete
// cancels the sweep's context and fails the way a cancelled request does, and
// SweepAuthState reports the ended context.
type cancelOnDeleteStore struct {
	sessionstore.Store
	cancel context.CancelFunc
}

func (s cancelOnDeleteStore) Delete(context.Context, tgid.UserID, string) error {
	s.cancel()
	return context.Canceled
}

func (s cancelOnDeleteStore) SweepAuthState(ctx context.Context, _ time.Time) ([]string, error) {
	return nil, fmt.Errorf("encrypted store: %w", ctx.Err())
}

// TestSweepStopsQuietlyOnShutdown pins that a sweep cut short by shutdown
// stops without logging the failures cancellation causes as errors.
func TestSweepStopsQuietlyOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	inner := sessionstoretest.NewWithClock(t, func() time.Time { return base })
	for _, sid := range []string{"0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"} {
		require.NoError(t, inner.Session(allowedUser, sid, sessionKey(t)).StoreSession(ctx, []byte("stale")))
	}
	a, logs := newLoggingServer(t, cancelOnDeleteStore{Store: inner, cancel: cancel})
	a.now = func() time.Time { return base.Add(refreshTokenTTL + sweepMargin + time.Minute) }

	a.runSweep(ctx)
	assert.NotContains(t, logs.String(), "level=ERROR")
}
