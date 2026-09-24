package sessionstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testGrantFamily = "fedcba9876543210fedcba9876543210"

// TestUndecodableGrantIsDead pins that a grant record that does not decode
// means what an absent one means: RotateGrant reports GrantMissing and
// RevokeGrant succeeds, rather than failing as a retryable error until the
// sweep deletes the record. The record is left for the sweep, not rewritten.
func TestUndecodableGrantIsDead(t *testing.T) {
	for name, b := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			writeRawGrant(t, b, testGrantFamily, "not-json")
			store := Encrypted(b, newCipher(t, testIssuer, newKey(t)))

			rotation, err := store.RotateGrant(ctx, testGrantFamily, 0, time.Now())
			require.NoError(t, err)
			assert.Equal(t, GrantMissing, rotation)
			require.NoError(t, store.RevokeGrant(ctx, testGrantFamily))

			_, _, err = b.LoadGrant(ctx, testGrantFamily)
			require.ErrorIs(t, err, errUndecodableGrant, "the record must be left for the sweep, not rewritten")
		})
	}
}

// TestGrantLoadsPersistedRecord pins the persisted grant JSON: a record written
// by an earlier build, including the session id it used to carry, still loads.
func TestGrantLoadsPersistedRecord(t *testing.T) {
	const persisted = `{"sid":"0123456789abcdef0123456789abcdef","generation":2,"expires_at":"2026-01-02T03:04:05Z","revoked":true,"write_id":"W"}`
	want := GrantRecord{Generation: 2, ExpiresAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Revoked: true, WriteID: "W"}

	fs := newTestFS(t)
	require.NoError(t, os.WriteFile(fs.grantPath(testGrantFamily), []byte(persisted), 0o600))
	gcs := NewTestGCS(t)
	w := gcs.bucket.Object(grantObjectName(testGrantFamily)).NewWriter(t.Context())
	_, err := io.WriteString(w, persisted)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	for name, b := range map[string]backend{"fs": fs, "gcs": gcs} {
		got, version, err := b.LoadGrant(t.Context(), testGrantFamily)
		require.NoError(t, err, name)
		assert.NotZero(t, version, name)
		assert.Equal(t, want, got, name)
	}
}

// TestGrantBackendCASContract pins the compare-and-swap every backend's
// StoreGrant must keep, step by step and without concurrency.
func TestGrantBackendCASContract(t *testing.T) {
	for name, b := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			first := GrantRecord{ExpiresAt: time.Now().Add(time.Hour).UTC()}
			require.NoError(t, b.StoreGrant(ctx, testGrantFamily, first, 0))
			require.ErrorIs(t, b.StoreGrant(ctx, testGrantFamily, first, 0), ErrGrantConflict,
				"version 0 creates only when no record exists")

			got, version, err := b.LoadGrant(ctx, testGrantFamily)
			require.NoError(t, err)
			require.NotZero(t, version)
			assert.Equal(t, first, got)

			second := first
			second.Generation = 1
			require.NoError(t, b.StoreGrant(ctx, testGrantFamily, second, version))
			third := second
			third.Generation = 2
			require.ErrorIs(t, b.StoreGrant(ctx, testGrantFamily, third, version), ErrGrantConflict,
				"a version that was already written over is stale")

			got, _, err = b.LoadGrant(ctx, testGrantFamily)
			require.NoError(t, err)
			assert.Equal(t, second, got)
		})
	}
}

// TestGrantWritesStampRecord pins the record the grant rules leave behind: the
// advanced generation, the expiry set at redemption and a WriteID.
func TestGrantWritesStampRecord(t *testing.T) {
	for name, b := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			store := Encrypted(b, newCipher(t, testIssuer, newKey(t)))
			now := time.Now()
			expiresAt := now.Add(time.Hour).UTC()
			created, err := store.RedeemCode(ctx, testGrantFamily, expiresAt)
			require.NoError(t, err)
			require.True(t, created)
			for gen := range int64(3) {
				result, err := store.RotateGrant(ctx, testGrantFamily, gen, now)
				require.NoError(t, err)
				require.Equal(t, GrantRotated, result, "generation %d", gen)
			}

			grant, _, err := b.LoadGrant(ctx, testGrantFamily)
			require.NoError(t, err)
			assert.NotEmpty(t, grant.WriteID, "every grant write stamps its record")
			assert.Equal(t, GrantRecord{Generation: 3, ExpiresAt: expiresAt, WriteID: grant.WriteID}, grant)
		})
	}
}

// badFamily is a grant family that lists before testGrantFamily.
const badFamily = "00000000000000000000000000000000"

// writeRawGrant stores data as family's grant record, bypassing encoding.
func writeRawGrant(t *testing.T, b backend, family, data string) {
	t.Helper()
	switch b := b.(type) {
	case *FS:
		require.NoError(t, os.WriteFile(b.grantPath(family), []byte(data), 0o600))
	case *GCS:
		w := b.bucket.Object(grantObjectName(family)).NewWriter(t.Context())
		_, err := io.WriteString(w, data)
		require.NoError(t, err)
		require.NoError(t, w.Close())
	default:
		t.Fatalf("unknown backend %T", b)
	}
}

// redeemExpired creates family's grant record already expired at now.
func redeemExpired(t *testing.T, store Store, family string, now time.Time) {
	t.Helper()
	created, err := store.RedeemCode(t.Context(), family, now.Add(-time.Minute))
	require.NoError(t, err)
	require.True(t, created)
}

// failGrantReads answers every read of family's grant object with a 403, an
// error the GCS client does not retry.
func failGrantReads(family string) func(http.RoundTripper) http.RoundTripper {
	return func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodGet && strings.Contains(req.URL.Path, family) {
				return forbidden(req), nil
			}
			return rt.RoundTrip(req)
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// forbidden is a 403 JSON API error response to req.
func forbidden(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":403,"message":"injected"}}`)),
		Request:    req,
	}
}

// TestGrantSweepDeletesUndecodableRecord pins that a grant record that cannot
// be decoded, and so can never rotate, is deleted and reported once instead
// of failing every sweep, and that the expired records after it are still
// swept.
func TestGrantSweepDeletesUndecodableRecord(t *testing.T) {
	for name, b := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now()
			writeRawGrant(t, b, badFamily, "not-json")
			store := Encrypted(b, newCipher(t, testIssuer, newKey(t)))
			redeemExpired(t, store, testGrantFamily, now)

			undecodable, err := store.SweepAuthState(ctx, now)
			require.NoError(t, err)
			assert.Equal(t, []string{badFamily}, undecodable)
			for _, family := range []string{badFamily, testGrantFamily} {
				_, version, err := b.LoadGrant(ctx, family)
				require.NoError(t, err)
				assert.Zero(t, version, "grant %s must be swept", family)
			}

			undecodable, err = store.SweepAuthState(ctx, now)
			require.NoError(t, err)
			assert.Empty(t, undecodable, "a deleted record is reported once")
		})
	}
}

// TestGrantSweepSkipsUnreadableRecord pins that a grant record that fails to
// read is left for the next sweep and reported in the error, without stalling
// the sweep of the expired records after it.
func TestGrantSweepSkipsUnreadableRecord(t *testing.T) {
	for name, b := range map[string]backend{"fs": newTestFS(t), "gcs": newTestGCSWithTransport(t, failGrantReads(badFamily))} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now()
			store := Encrypted(b, newCipher(t, testIssuer, newKey(t)))
			redeemExpired(t, store, badFamily, now)
			redeemExpired(t, store, testGrantFamily, now)
			if fs, ok := b.(*FS); ok {
				require.NoError(t, os.Chmod(fs.grantPath(badFamily), 0))
				t.Cleanup(func() { _ = os.Chmod(fs.grantPath(badFamily), 0o600) })
			}

			undecodable, err := store.SweepAuthState(ctx, now)
			require.ErrorContains(t, err, "sweeping grant "+badFamily)
			assert.NotErrorIs(t, err, errUndecodableGrant)
			assert.Empty(t, undecodable)
			_, version, err := b.LoadGrant(ctx, testGrantFamily)
			require.NoError(t, err)
			assert.Zero(t, version, "the expired grant after the unreadable one must still be swept")

			if fs, ok := b.(*FS); ok {
				require.NoError(t, os.Chmod(fs.grantPath(badFamily), 0o600))
				_, version, err := b.LoadGrant(ctx, badFamily)
				require.NoError(t, err)
				assert.NotZero(t, version, "an unreadable record must be kept for the next sweep")
			}
		})
	}
}

// TestGrantSweepStopsWhenContextEnds pins that a sweep whose context has ended
// deletes nothing more and returns the context's error.
func TestGrantSweepStopsWhenContextEnds(t *testing.T) {
	for name, b := range backends(t) {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			redeemExpired(t, Encrypted(b, newCipher(t, testIssuer, newKey(t))), testGrantFamily, now)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			_, err := b.SweepAuthState(ctx, now)
			require.ErrorIs(t, err, context.Canceled)
			_, version, err := b.LoadGrant(t.Context(), testGrantFamily)
			require.NoError(t, err)
			assert.NotZero(t, version)
		})
	}
}

// TestGCSGrantSweepStopsMidListingKeepingFailures pins that a context ending
// part-way through a GCS sweep stops it without dropping what it already
// collected: a real failure before the cancellation comes back joined with
// the context's error.
func TestGCSGrantSweepStopsMidListingKeepingFailures(t *testing.T) {
	const cutFamily = "11111111111111111111111111111111"
	ctx, cancel := context.WithCancel(t.Context())
	b := newTestGCSWithTransport(t, func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				return rt.RoundTrip(req)
			}
			switch {
			case strings.Contains(req.URL.Path, badFamily):
				return forbidden(req), nil
			case strings.Contains(req.URL.Path, cutFamily):
				cancel()
				return nil, context.Canceled
			}
			return rt.RoundTrip(req)
		})
	})
	now := time.Now()
	store := Encrypted(b, newCipher(t, testIssuer, newKey(t)))
	for _, family := range []string{badFamily, cutFamily, testGrantFamily} {
		redeemExpired(t, store, family, now)
	}

	_, err := b.SweepAuthState(ctx, now)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "sweeping grant "+badFamily, "a failure before the cancellation must be kept")
	_, version, err := b.LoadGrant(t.Context(), testGrantFamily)
	require.NoError(t, err)
	assert.NotZero(t, version, "the sweep must stop at the cancellation")
}

// TestGrantRotationZeroIsRefusal pins that an outcome nobody set cannot pass
// the token endpoint's success check.
func TestGrantRotationZeroIsRefusal(t *testing.T) {
	var unset GrantRotation
	assert.NotEqual(t, GrantRotated, unset)
}

// alwaysConflicting loses every grant write to a concurrent writer.
type alwaysConflicting struct {
	backend
	loads, stores int
}

func (s *alwaysConflicting) LoadGrant(ctx context.Context, family string) (GrantRecord, int64, error) {
	s.loads++
	return s.backend.LoadGrant(ctx, family)
}

func (s *alwaysConflicting) StoreGrant(context.Context, string, GrantRecord, int64) error {
	s.stores++
	return ErrGrantConflict
}

// TestUpdateGrantGivesUp pins that a grant under constant contention fails
// with ErrGrantConflict after four rounds instead of spinning. Each round
// loads once to compute the write and once to check whether the refused write
// landed after all.
func TestUpdateGrantGivesUp(t *testing.T) {
	ctx := t.Context()
	fs := newTestFS(t)
	cipher := newCipher(t, testIssuer, newKey(t))
	created, err := Encrypted(fs, cipher).RedeemCode(ctx, testGrantFamily, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.True(t, created)

	b := &alwaysConflicting{backend: fs}
	_, err = Encrypted(b, cipher).RotateGrant(ctx, testGrantFamily, 0, time.Now())
	require.ErrorIs(t, err, ErrGrantConflict)
	assert.Equal(t, 8, b.loads)
	assert.Equal(t, 4, b.stores)
}

// unreadableAfterWrite fails a grant write, and every read after it, with
// its own errors.
type unreadableAfterWrite struct {
	backend
	storeErr, loadErr error
	wrote             bool
}

func (s *unreadableAfterWrite) LoadGrant(ctx context.Context, family string) (GrantRecord, int64, error) {
	if s.wrote {
		return GrantRecord{}, 0, s.loadErr
	}
	return s.backend.LoadGrant(ctx, family)
}

func (s *unreadableAfterWrite) StoreGrant(context.Context, string, GrantRecord, int64) error {
	s.wrote = true
	return s.storeErr
}

// TestWriteGrantReportsAnUnknownOutcome pins that a failed grant write whose
// re-read fails too reports both errors — the write may have landed — rather
// than only the write's, or a lost race to retry.
func TestWriteGrantReportsAnUnknownOutcome(t *testing.T) {
	ctx := t.Context()
	fs := newTestFS(t)
	cipher := newCipher(t, testIssuer, newKey(t))
	created, err := Encrypted(fs, cipher).RedeemCode(ctx, testGrantFamily, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.True(t, created)

	for _, storeErr := range []error{ErrGrantConflict, errors.New("connection reset by peer")} {
		b := &unreadableAfterWrite{backend: fs, storeErr: storeErr, loadErr: errors.New("bucket unavailable")}
		_, err := Encrypted(b, cipher).RotateGrant(ctx, testGrantFamily, 0, time.Now())
		require.ErrorIs(t, err, storeErr)
		require.ErrorIs(t, err, b.loadErr)
		assert.ErrorContains(t, err, "outcome unknown")
	}
}

// notLanded fails every grant write with err before it reaches storage.
type notLanded struct {
	backend
	err    error
	stores int
}

func (s *notLanded) StoreGrant(context.Context, string, GrantRecord, int64) error {
	s.stores++
	return s.err
}

// TestGrantWriteThatDidNotLandFails pins that a grant write failing with a
// transport error, whose re-read finds no record or another write's, reports
// that error once — not a lost race, which RedeemCode would turn into "code
// already redeemed" and RotateGrant into a retry that could revoke the family.
func TestGrantWriteThatDidNotLandFails(t *testing.T) {
	for name, b := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now()
			cipher := newCipher(t, testIssuer, newKey(t))
			store := Encrypted(b, cipher)

			redeem := &notLanded{backend: b, err: errors.New("connection reset by peer")}
			created, err := Encrypted(redeem, cipher).RedeemCode(ctx, testGrantFamily, now.Add(time.Hour))
			require.ErrorIs(t, err, redeem.err)
			assert.False(t, created)
			assert.Equal(t, 1, redeem.stores, "a failed write must not be retried")

			created, err = store.RedeemCode(ctx, testGrantFamily, now.Add(time.Hour))
			require.NoError(t, err)
			require.True(t, created, "the failed redemption must not consume the code")

			rotate := &notLanded{backend: b, err: errors.New("connection reset by peer")}
			result, err := Encrypted(rotate, cipher).RotateGrant(ctx, testGrantFamily, 0, now)
			require.ErrorIs(t, err, rotate.err)
			assert.Equal(t, GrantMissing, result)
			assert.Equal(t, 1, rotate.stores, "a failed write must not be retried")

			result, err = store.RotateGrant(ctx, testGrantFamily, 0, now)
			require.NoError(t, err)
			assert.Equal(t, GrantRotated, result, "the failed rotation must leave the family as it was")
		})
	}
}

// lostResponse lands the first grant write it is given, then reports that
// write as failed with err, the way a write whose response never arrived does.
type lostResponse struct {
	backend
	err  error
	lost bool
}

func (s *lostResponse) StoreGrant(ctx context.Context, family string, grant GrantRecord, version int64) error {
	if err := s.backend.StoreGrant(ctx, family, grant, version); err != nil {
		return err
	}
	if s.lost {
		return nil
	}
	s.lost = true
	return s.err
}

// TestGrantWriteSurvivesLostResponse pins that a grant write which landed but
// reported failure — a GCS retry answered 412 after the first attempt
// committed, or a transport error after the commit — counts as done, instead
// of reading the advanced record as someone else's and revoking the family.
func TestGrantWriteSurvivesLostResponse(t *testing.T) {
	failures := map[string]error{
		"conflict":  ErrGrantConflict,
		"transport": errors.New("connection reset by peer"),
	}
	for failure, failErr := range failures {
		for name, b := range backends(t) {
			t.Run(failure+"/"+name, func(t *testing.T) {
				ctx := t.Context()
				now := time.Now()
				cipher := newCipher(t, testIssuer, newKey(t))
				store := Encrypted(b, cipher)
				lost := func() Store { return Encrypted(&lostResponse{backend: b, err: failErr}, cipher) }

				created, err := lost().RedeemCode(ctx, testGrantFamily, now.Add(time.Hour))
				require.NoError(t, err)
				assert.True(t, created, "the code's own landed write is a redemption, not a reuse")

				result, err := lost().RotateGrant(ctx, testGrantFamily, 0, now)
				require.NoError(t, err)
				assert.Equal(t, GrantRotated, result, "the rotation's own landed write is not a replay")

				result, err = store.RotateGrant(ctx, testGrantFamily, 1, now)
				require.NoError(t, err)
				assert.Equal(t, GrantRotated, result, "the family must survive the lost response")

				require.NoError(t, lost().RevokeGrant(ctx, testGrantFamily))
				result, err = store.RotateGrant(ctx, testGrantFamily, 2, now)
				require.NoError(t, err)
				assert.Equal(t, GrantReplay, result, "a revocation whose response was lost still revokes")
			})
		}
	}
}

// TestGCSGrantSweepJoinsListingFailure pins that a listing that fails part-way
// through a GCS sweep is reported together with the failures already seen.
func TestGCSGrantSweepJoinsListingFailure(t *testing.T) {
	b := newTestGCSWithTransport(t, func(rt http.RoundTripper) http.RoundTripper {
		failRead := failGrantReads(badFamily)(rt)
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/o") {
				return failRead.RoundTrip(req)
			}
			// A listing: serve one object per page and fail the second page.
			query := req.URL.Query()
			if query.Get("pageToken") != "" {
				return forbidden(req), nil
			}
			query.Set("maxResults", "1")
			req = req.Clone(req.Context())
			req.URL.RawQuery = query.Encode()
			return rt.RoundTrip(req)
		})
	})
	now := time.Now()
	store := Encrypted(b, newCipher(t, testIssuer, newKey(t)))
	redeemExpired(t, store, badFamily, now)
	redeemExpired(t, store, testGrantFamily, now)

	_, err := store.SweepAuthState(t.Context(), now)
	require.ErrorContains(t, err, "sweeping grant "+badFamily)
	require.ErrorContains(t, err, "listing grants")
}
