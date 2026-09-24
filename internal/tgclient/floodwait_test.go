package tgclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
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

// waitEvent is one wait a flood-wait middleware logs.
type waitEvent struct {
	msg   string
	level slog.Level
	wait  time.Duration
}

// waitLog is a slog handler recording the waits a flood-wait middleware logs.
type waitLog struct {
	mu     sync.Mutex
	events []waitEvent
}

func (*waitLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *waitLog) WithAttrs([]slog.Attr) slog.Handler     { return l }
func (l *waitLog) WithGroup(string) slog.Handler          { return l }

func (l *waitLog) Handle(_ context.Context, rec slog.Record) error {
	event := waitEvent{msg: rec.Message, level: rec.Level}
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "wait_seconds" {
			event.wait = time.Duration(a.Value.Float64() * float64(time.Second))
		}
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
	return nil
}

// waits returns the waits logged with msg.
func (l *waitLog) waits(msg string) []time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	var waits []time.Duration
	for _, e := range l.events {
		if e.msg == msg {
			waits = append(waits, e.wait)
		}
	}
	return waits
}

const (
	waitingOut = "telegram told a call to wait; waiting it out"
	failedFast = "telegram told a call to wait past the maximum; failing fast with a retry-after"
)

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

		require.NoError(t, ping(t.Context(), floodWait(time.Minute, slog.New(&log)).Handle(script)))
		assert.Equal(t, int32(3), script.tries.Load(), "the call is retried after each wait")
		assert.Equal(t, []time.Duration{3 * time.Second, time.Second}, log.waits(waitingOut), "a FLOOD_WAIT_0 waits the one-second minimum")
		assert.Equal(t, 4*time.Second, time.Since(start), "the call sleeps each wait out")
	})
}

func TestFloodWaitReturnsAWaitPastTheMaximumAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		script := &floodScript{errs: []error{tgerr.New(420, "FLOOD_WAIT_265")}}
		var log waitLog
		start := time.Now()

		err := ping(t.Context(), floodWait(time.Minute, slog.New(&log)).Handle(script))
		d, ok := tgerr.AsFloodWait(err)
		require.True(t, ok, "Telegram's error survives for the tools' retry-after: %v", err)
		assert.Equal(t, 265*time.Second, d)
		assert.Equal(t, []time.Duration{265 * time.Second}, log.waits(failedFast), "the wait the call fails fast on is logged")
		assert.Equal(t, int32(1), script.tries.Load(), "the call is not retried")
		assert.Zero(t, time.Since(start), "the call does not sleep")
	})
}

// TestFloodWaitCapsTheTotalWaitOfAToolCall pins that maxWait bounds what the
// calls on one budget wait in all, not each wait or each call: the waits
// that fit are slept out, and the one that would take them past maxWait is
// returned at once. Calls without a budget each have one of their own.
func TestFloodWaitCapsTheTotalWaitOfAToolCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		flood := func() tg.Invoker {
			return &floodScript{errs: []error{tgerr.New(420, "FLOOD_WAIT_30")}}
		}
		var log waitLog
		mw := floodWait(time.Minute, slog.New(&log))
		start := time.Now()

		toolCall := WithWaitBudget(t.Context())
		require.NoError(t, ping(toolCall, mw.Handle(flood())))
		require.NoError(t, ping(toolCall, mw.Handle(flood())))
		err := ping(toolCall, mw.Handle(flood()))
		require.True(t, tgerr.Is(err, tgerr.ErrFloodWait), "Telegram's error survives for the tools' retry-after: %v", err)
		assert.ErrorContains(t, err, "with the 1m0s already waited passes the 1m0s maximum")
		assert.Equal(t, time.Minute, time.Since(start), "the tool call waits no more than maxWait in all")
		assert.Equal(t, []time.Duration{30 * time.Second}, log.waits(failedFast))

		for range 3 {
			require.NoError(t, ping(t.Context(), mw.Handle(flood())), "a call without a budget has one of its own")
		}
	})
}

// TestFloodWaitBacksOffOnAWaitWithoutALength pins the waits Telegram gives no
// length for: the call backs off from a second, doubling, and gives up after
// maxUnstatedRetries, returning Telegram's error.
func TestFloodWaitBacksOffOnAWaitWithoutALength(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		busy := tgerr.New(500, "WORKER_BUSY_TOO_LONG_RETRY")
		script := &floodScript{errs: []error{busy, busy, busy, busy}}
		var log waitLog
		start := time.Now()

		err := ping(t.Context(), floodWait(time.Minute, slog.New(&log)).Handle(script))
		require.ErrorIs(t, err, busy, "Telegram's error survives for the tools' retry-after")
		assert.ErrorContains(t, err, "4 times in a row")
		assert.Equal(t, int32(4), script.tries.Load())
		assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, log.waits(waitingOut))
		assert.Equal(t, 7*time.Second, time.Since(start))

		skip := tgerr.New(420, "FLOOD_SKIP_FAILED_WAIT")
		script = &floodScript{errs: []error{skip, skip}}
		require.NoError(t, ping(t.Context(), floodWait(time.Minute, slog.New(&log)).Handle(script)), "a hiccup that passes is retried")
		assert.Equal(t, int32(3), script.tries.Load())
	})
}

// TestFloodWaitLogsSlowModeAtInfo pins the log levels: one chat's slow mode
// is that chat's own pace, the others hold up every call alike.
func TestFloodWaitLogsSlowModeAtInfo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var log waitLog
		script := &floodScript{errs: []error{tgerr.New(420, "SLOWMODE_WAIT_1"), tgerr.New(420, "FLOOD_WAIT_1")}}
		require.NoError(t, ping(t.Context(), floodWait(time.Minute, slog.New(&log)).Handle(script)))
		require.Len(t, log.events, 2)
		assert.Equal(t, slog.LevelInfo, log.events[0].level)
		assert.Equal(t, slog.LevelWarn, log.events[1].level)
	})
}

func TestFloodWaitStopsSleepingWhenTheCallIsCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		flood := tgerr.New(420, "FLOOD_WAIT_30")
		script := &floodScript{errs: []error{flood}}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		start := time.Now()
		go func() { done <- ping(ctx, floodWait(time.Minute, discardLogger()).Handle(script)) }()

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
		invoker := floodWait(time.Minute, discardLogger()).Handle(telegram.InvokeFunc(func(_ context.Context, input bin.Encoder, _ bin.Decoder) error {
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
	waits := map[*tgerr.Error]Wait{
		tgerr.New(420, "FLOOD_WAIT_7"):               {7 * time.Second, ScopeAccount, "FLOOD_WAIT", true},
		tgerr.New(420, "FLOOD_PREMIUM_WAIT_5"):       {5 * time.Second, ScopeAccount, "FLOOD_PREMIUM_WAIT", true},
		tgerr.New(420, "FLOOD_TEST_PHONE_WAIT_2"):    {2 * time.Second, ScopeAccount, "FLOOD_TEST_PHONE_WAIT", true},
		tgerr.New(420, "FLOOD_WAIT_0"):               {time.Second, ScopeAccount, "FLOOD_WAIT", true},
		tgerr.New(420, "FLOOD_WAIT_2147483647"):      {2147483647 * time.Second, ScopeAccount, "FLOOD_WAIT", true},
		tgerr.New(420, "FLOOD_SKIP_FAILED_WAIT"):     {time.Second, ScopeAccount, "FLOOD_SKIP_FAILED_WAIT", false},
		tgerr.New(420, "SLOWMODE_WAIT_10"):           {10 * time.Second, ScopeChat, "SLOWMODE_WAIT", true},
		tgerr.New(500, "WORKER_BUSY_TOO_LONG_RETRY"): {time.Second, ScopeServer, "WORKER_BUSY_TOO_LONG_RETRY", false},
		tgerr.New(420, "TAKEOUT_INIT_DELAY_3600"):    {time.Hour, ScopeServer, "TAKEOUT_INIT_DELAY", true},
		tgerr.New(420, "2FA_CONFIRM_WAIT_604800"):    {7 * 24 * time.Hour, ScopeServer, "2FA_CONFIRM_WAIT", true},
	}
	for rpcErr, want := range waits {
		got, ok := RetryAfter(fmt.Errorf("wrapped: %w", rpcErr))
		assert.True(t, ok, rpcErr.Message)
		assert.Equal(t, want, got, rpcErr.Message)
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
