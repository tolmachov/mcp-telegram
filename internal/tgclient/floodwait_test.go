package tgclient

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// floodScript answers the n-th try of a call with the n-th of errs, and every
// try past them with success, counting the tries.
type floodScript struct {
	errs  []error
	tries atomic.Int32
}

func (s *floodScript) Invoke(context.Context, bin.Encoder, bin.Decoder) error {
	n := int(s.tries.Add(1))
	if n <= len(s.errs) {
		return s.errs[n-1]
	}
	return nil
}

// waitEvent is one wait a flood-wait middleware reports.
type waitEvent struct {
	wait     time.Duration
	retrying bool
}

// waitLog records the waits a flood-wait middleware reports.
type waitLog struct{ events []waitEvent }

func (l *waitLog) record(_ context.Context, wait time.Duration, retrying bool) {
	l.events = append(l.events, waitEvent{wait, retrying})
}

// ping makes one call through invoker.
func ping(ctx context.Context, invoker tg.Invoker) error {
	_, err := tg.NewClient(invoker).HelpGetNearestDC(ctx)
	return err //nolint:wrapcheck // the test inspects the middleware's reply as is.
}

func TestFloodWaitSleepsAndRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		script := &floodScript{errs: []error{tgerr.New(420, "FLOOD_WAIT_3"), tgerr.New(420, "FLOOD_WAIT_0")}}
		var log waitLog
		start := time.Now()

		require.NoError(t, ping(t.Context(), floodWait(time.Minute, log.record).Handle(script)))
		assert.Equal(t, int32(3), script.tries.Load(), "the call is retried after each wait")
		assert.Equal(t, []waitEvent{{3 * time.Second, true}, {time.Second, true}}, log.events, "a FLOOD_WAIT_0 waits the one-second minimum")
		assert.Equal(t, 4*time.Second, time.Since(start), "the call sleeps each wait out")
	})
}

func TestFloodWaitReturnsAWaitPastTheMaximumAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		script := &floodScript{errs: []error{tgerr.New(420, "FLOOD_WAIT_265")}}
		var log waitLog
		start := time.Now()

		err := ping(t.Context(), floodWait(time.Minute, log.record).Handle(script))
		d, ok := tgerr.AsFloodWait(err)
		require.True(t, ok, "Telegram's error survives for the tools' retry-after: %v", err)
		assert.Equal(t, 265*time.Second, d)
		assert.Equal(t, []waitEvent{{265 * time.Second, false}}, log.events, "the callback hears of the wait it fails fast on")
		assert.Equal(t, int32(1), script.tries.Load(), "the call is not retried")
		assert.Zero(t, time.Since(start), "the call does not sleep")
	})
}

// TestFloodWaitCapsTheTotalWaitOfACall pins that maxWait bounds what one call
// waits in all, not each wait: the waits that fit are slept out, and the one
// that would take the call past maxWait is returned at once and reported as
// given up on.
func TestFloodWaitCapsTheTotalWaitOfACall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		busy := tgerr.New(500, "WORKER_BUSY_TOO_LONG_RETRY")
		script := &floodScript{errs: []error{tgerr.New(420, "FLOOD_WAIT_30"), tgerr.New(420, "FLOOD_WAIT_30"), busy}}
		var log waitLog
		start := time.Now()

		err := ping(t.Context(), floodWait(time.Minute, log.record).Handle(script))
		require.ErrorIs(t, err, busy, "Telegram's error survives for the tools' retry-after")
		assert.ErrorContains(t, err, "with the 1m0s already waited passes the 1m0s a call waits out")
		assert.Equal(t, int32(3), script.tries.Load())
		assert.Equal(t, []waitEvent{{30 * time.Second, true}, {30 * time.Second, true}, {time.Second, false}}, log.events,
			"the callback hears of the wait the call gives up on too")
		assert.Equal(t, time.Minute, time.Since(start), "the call waits no more than maxWait in all")
	})
}

func TestFloodWaitStopsSleepingWhenTheCallIsCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		flood := tgerr.New(420, "FLOOD_WAIT_30")
		script := &floodScript{errs: []error{flood}}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		start := time.Now()
		go func() { done <- ping(ctx, floodWait(time.Minute, nil).Handle(script)) }()

		synctest.Wait()
		cancel()
		err := <-done
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, err, flood, "Telegram's error survives for the tools' retry-after")
		assert.Zero(t, time.Since(start), "the call returns without sleeping the wait out")
		assert.Equal(t, int32(1), script.tries.Load())
	})
}

// TestFloodWaitHoldsUpNoOtherCall pins that one call sleeping on a flood wait
// leaves the others to go ahead, the gotd call that makes FILE_MIGRATE work
// included (see TestStartClientDownloadsFromAnotherDC).
func TestFloodWaitHoldsUpNoOtherCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var floodedOnce atomic.Bool
		invoker := floodWait(time.Minute, nil).Handle(telegram.InvokeFunc(func(_ context.Context, input bin.Encoder, _ bin.Decoder) error {
			if _, ok := input.(*tg.HelpGetNearestDCRequest); ok && floodedOnce.CompareAndSwap(false, true) {
				return tgerr.New(420, "FLOOD_WAIT_30")
			}
			return nil
		}))
		waiting := make(chan error, 1)
		go func() { waiting <- ping(t.Context(), invoker) }()
		synctest.Wait()

		_, err := tg.NewClient(invoker).HelpGetConfig(t.Context())
		require.NoError(t, err)
		select {
		case err := <-waiting:
			t.Fatalf("the waiting call returned before its wait: %v", err)
		default:
		}

		require.NoError(t, <-waiting, "the waiting call is retried after its wait")
	})
}

func TestRetryAfterRecognisesTelegramsWaits(t *testing.T) {
	waits := map[*tgerr.Error]time.Duration{
		tgerr.New(420, "FLOOD_WAIT_7"):               7 * time.Second,
		tgerr.New(420, "FLOOD_PREMIUM_WAIT_5"):       5 * time.Second,
		tgerr.New(420, "SLOWMODE_WAIT_10"):           10 * time.Second,
		tgerr.New(420, "FLOOD_WAIT_0"):               time.Second,
		tgerr.New(420, "FLOOD_SKIP_FAILED_WAIT"):     time.Second,
		tgerr.New(500, "WORKER_BUSY_TOO_LONG_RETRY"): time.Second,
		tgerr.New(420, "FLOOD_TEST_PHONE_WAIT_2"):    2 * time.Second,
		tgerr.New(420, "TAKEOUT_INIT_DELAY_3600"):    time.Hour,
		tgerr.New(420, "2FA_CONFIRM_WAIT_604800"):    7 * 24 * time.Hour,
		tgerr.New(420, "FLOOD_WAIT_2147483647"):      2147483647 * time.Second,
	}
	for rpcErr, want := range waits {
		d, ok := RetryAfter(fmt.Errorf("wrapped: %w", rpcErr))
		assert.True(t, ok, rpcErr.Message)
		assert.Equal(t, want, d, rpcErr.Message)
	}

	for _, err := range []error{
		nil,
		errors.New("dial tcp: i/o timeout"),
		tgerr.New(303, "FILE_MIGRATE_4"),
		tgerr.New(420, "SOMETHING_NEW_5"),
		tgerr.New(500, "INTERNAL_SERVER_ERROR"),
	} {
		_, ok := RetryAfter(err)
		assert.False(t, ok, "%v", err)
	}
}
