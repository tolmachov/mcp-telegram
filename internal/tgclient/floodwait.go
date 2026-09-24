package tgclient

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// FloodWaitCallback is invoked each time Telegram tells a call to wait (see
// RetryAfter), with how long, and whether the call waits it out and retries —
// it does while the waits it takes add up to no more than the client's
// FloodWaitMaxWait — or gives up and returns Telegram's error. Use it to
// surface throttling so users understand why a tool is slow or failed.
type FloodWaitCallback func(ctx context.Context, wait time.Duration, retrying bool)

// floodWait is the middleware that waits out the waits Telegram tells a call
// to take, inside that call: each call sleeps on its own and retries, so a
// call that waits holds up no other. Nothing else would do: gotd makes calls
// of its own through the middlewares while a call is inside them — moving the
// session's authorisation to the DC a FILE_MIGRATE names, for one — so a
// middleware that sends calls one at a time deadlocks on them.
//
// maxWait is what one call may wait in all, so that it returns within the
// MCP client's tool-call timeout: a wait that would take the call past it is
// returned at once, wrapping Telegram's error so tools can render its
// retry-after. A call whose context ends while it waits returns both. onWait,
// if non-nil, is told of every wait, the ones the call gives up on included.
func floodWait(maxWait time.Duration, onWait FloodWaitCallback) telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			var waited time.Duration
			for {
				err := next.Invoke(ctx, input, output)
				wait, ok := RetryAfter(err)
				if !ok {
					return err //nolint:wrapcheck // a middleware passes Telegram's reply through unchanged.
				}
				retrying := waited+wait <= maxWait
				if onWait != nil {
					onWait(ctx, wait, retrying)
				}
				if !retrying {
					return fmt.Errorf("telegram asked for a wait of %s, which with the %s already waited passes the %s a call waits out: %w", wait, waited, maxWait, err)
				}
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
					waited += wait
				case <-ctx.Done():
					timer.Stop()
					return fmt.Errorf("%w while waiting out the %s Telegram asked for (%w)", ctx.Err(), wait, err)
				}
			}
		}
	})
}

// RetryAfter reports how long err tells its call to wait before retrying — at
// least a second — and whether it is such an error at all, wrapped or not.
// The set follows TDLib's NetQueryDelayer (as gotd/contrib's floodwait does):
// the 420 waits that carry their length, a FLOOD_WAIT_0 raised to the
// one-second minimum, and two errors that carry none and are retried after
// that minimum.
func RetryAfter(err error) (time.Duration, bool) {
	rpcErr, ok := tgerr.As(err)
	if !ok {
		return 0, false
	}
	switch {
	case rpcErr.Code == 420 && rpcErr.IsOneOf(tgerr.ErrFloodWait, tgerr.ErrPremiumFloodWait,
		"SLOWMODE_WAIT", "2FA_CONFIRM_WAIT", "TAKEOUT_INIT_DELAY", "FLOOD_TEST_PHONE_WAIT"):
		return time.Duration(max(rpcErr.Argument, 1)) * time.Second, true
	case rpcErr.Code == 420 && rpcErr.IsType("FLOOD_SKIP_FAILED_WAIT"),
		rpcErr.Code == 500 && rpcErr.IsType("WORKER_BUSY_TOO_LONG_RETRY"):
		return time.Second, true
	default:
		return 0, false
	}
}
