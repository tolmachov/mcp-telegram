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

// floodWaitRetries is how many waits one call sits out before the next wait
// it is told to take is returned as its error.
const floodWaitRetries = 5

// FloodWaitCallback is invoked when Telegram tells a call to wait and the call
// has waits left, with how long it was told to wait: the call sleeps that long
// and retries when the wait is within the client's FloodWaitMaxWait, and
// returns Telegram's error otherwise. Use it to surface throttling to the MCP
// client (via mcpLog at warning level) so users understand why a tool is slow.
type FloodWaitCallback func(ctx context.Context, duration time.Duration)

// floodWait is the middleware that waits out the waits Telegram tells a call
// to take, inside that call: each call sleeps on its own and retries, so a
// call that waits holds up no other. Nothing else would do: gotd makes calls
// of its own through the middlewares while a call is inside them — moving the
// session's authorisation to the DC a FILE_MIGRATE names, for one — so a
// middleware that sends calls one at a time deadlocks on them.
//
// A wait longer than maxWait, or one past the call's floodWaitRetries, is
// returned at once, wrapping Telegram's error so tools can render its
// retry-after. onWait, if non-nil, is told of every wait the call does not
// give up on for its retries (see FloodWaitCallback).
func floodWait(maxWait time.Duration, onWait FloodWaitCallback) telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			for waited := 0; ; waited++ {
				err := next.Invoke(ctx, input, output)
				wait, ok := floodWaitOf(err)
				switch {
				case !ok:
					return err //nolint:wrapcheck // a middleware passes Telegram's reply through unchanged.
				case waited == floodWaitRetries:
					return fmt.Errorf("giving up after waiting out %d flood waits: %w", waited, err)
				}
				if onWait != nil {
					onWait(ctx, wait)
				}
				if wait > maxWait {
					return fmt.Errorf("flood wait of %s exceeds the %s the client waits out: %w", wait, maxWait, err)
				}
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return fmt.Errorf("waiting out a flood wait of %s: %w", wait, ctx.Err())
				}
			}
		}
	})
}

// floodWaitOf reports how long err tells its call to wait before retrying, and
// whether it is such an error at all. The set follows TDLib's NetQueryDelayer
// (as gotd/contrib's floodwait does): the 420 waits that carry their length,
// and two errors that carry none and are retried after the one-second minimum
// every wait is raised to — a FLOOD_WAIT_0 included.
func floodWaitOf(err error) (time.Duration, bool) {
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
