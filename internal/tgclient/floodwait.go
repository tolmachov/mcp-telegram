package tgclient

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// WaitScope is what a wait Telegram tells a call to take is a condition of.
type WaitScope uint8

const (
	// ScopeAccount is an account-level flood limit: cumulative actions over
	// a window, not request rate, so only spacing the calls out avoids it.
	ScopeAccount WaitScope = iota + 1
	// ScopeChat is one chat's slow mode: a limit on sending there alone.
	ScopeChat
	// ScopeServer is a delay Telegram's servers put on the request itself —
	// busy workers, or a security or takeout delay — which says nothing about
	// the account's pace or any chat.
	ScopeServer
)

// Wait is a wait Telegram told a call to take before retrying it.
type Wait struct {
	// Duration is how long to wait: at least a second.
	Duration time.Duration
	Scope    WaitScope
	// Type is Telegram's error type, e.g. FLOOD_WAIT.
	Type string
	// Stated is whether Telegram gave the length. A wait without one is
	// Duration's one-second minimum, and the flood-wait middleware backs off
	// on it instead (see floodWait).
	Stated bool
}

// RetryAfter reports the wait err tells its call to take, wrapped or not, and
// whether it is such an error at all. The set follows TDLib's NetQueryDelayer
// (as gotd/contrib's floodwait does): the 420 waits that carry their length, a
// FLOOD_WAIT_0 raised to the one-second minimum, and two errors that carry no
// length.
func RetryAfter(err error) (Wait, bool) {
	rpcErr, ok := tgerr.As(err)
	if !ok {
		return Wait{}, false
	}
	wait := Wait{Duration: time.Duration(max(rpcErr.Argument, 1)) * time.Second, Type: rpcErr.Type, Stated: true}
	switch {
	case rpcErr.Code == 420 && rpcErr.IsOneOf(tgerr.ErrFloodWait, tgerr.ErrPremiumFloodWait, "FLOOD_TEST_PHONE_WAIT"):
		wait.Scope = ScopeAccount
	case rpcErr.Code == 420 && rpcErr.IsType("SLOWMODE_WAIT"):
		wait.Scope = ScopeChat
	case rpcErr.Code == 420 && rpcErr.IsOneOf("2FA_CONFIRM_WAIT", "TAKEOUT_INIT_DELAY"):
		wait.Scope = ScopeServer
	case rpcErr.Code == 420 && rpcErr.IsType("FLOOD_SKIP_FAILED_WAIT"):
		wait.Scope, wait.Duration, wait.Stated = ScopeAccount, time.Second, false
	case rpcErr.Code == 500 && rpcErr.IsType("WORKER_BUSY_TOO_LONG_RETRY"):
		wait.Scope, wait.Duration, wait.Stated = ScopeServer, time.Second, false
	default:
		return Wait{}, false
	}
	return wait, true
}

// maxUnstatedRetries is how many times in a row one call retries a wait
// Telegram gave no length for (Wait.Stated), backing off from a second and
// doubling: an answer that repeats past that is not a short hiccup.
const maxUnstatedRetries = 3

// waitBudget is the total wait the calls on one context have taken.
type waitBudget struct {
	mu     sync.Mutex
	waited time.Duration
}

// take adds d to the budget unless that would take it past maxWait, and
// returns what had been waited before.
func (b *waitBudget) take(d, maxWait time.Duration) (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	waited := b.waited
	if waited+d > maxWait {
		return waited, false
	}
	b.waited += d
	return waited, true
}

type waitBudgetKey struct{}

// WithWaitBudget returns ctx with a flood-wait budget of its own: every call
// made on it, or on a context derived from it, draws on one total, so that all
// of them together wait no more than Config.FloodWaitMaxWait on the waits
// Telegram tells them to take. One tool call is what attaches it: the MCP
// client's timeout is on the tool call, not on each of its Telegram calls. A
// call on a context without a budget — a background load — has one of its
// own.
func WithWaitBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, waitBudgetKey{}, &waitBudget{})
}

// budgetOf returns the budget ctx carries, or a fresh one for this call alone.
func budgetOf(ctx context.Context) *waitBudget {
	if b, ok := ctx.Value(waitBudgetKey{}).(*waitBudget); ok {
		return b
	}
	return &waitBudget{}
}

// floodWait is the middleware that waits out the waits Telegram tells a call
// to take, inside that call: each call sleeps on its own and retries, so a
// call that waits holds up no other. Nothing else would do: gotd makes calls
// of its own through the middlewares while a call is inside them — moving the
// session's authorisation to the DC a FILE_MIGRATE names, for one — so a
// middleware that sends calls one at a time deadlocks on them.
//
// maxWait is what the calls sharing a budget (WithWaitBudget) may wait in
// all, so that a tool call returns within the MCP client's timeout: a wait
// that would take them past it is returned at once, wrapping Telegram's error
// so tools can render its retry-after. A wait without a length is backed off
// and given up after maxUnstatedRetries. A call whose context ends while it
// waits returns both. Every wait is logged through logger, the ones given up
// on included.
func floodWait(maxWait time.Duration, logger *slog.Logger) telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			budget := budgetOf(ctx)
			unstated := 0
			for {
				err := next.Invoke(ctx, input, output)
				wait, ok := RetryAfter(err)
				if !ok {
					return err //nolint:wrapcheck // a middleware passes Telegram's reply through unchanged.
				}
				d := wait.Duration
				if !wait.Stated {
					if unstated == maxUnstatedRetries {
						logWait(ctx, logger, wait, d, "telegram kept telling a call to wait without saying how long; failing")
						return fmt.Errorf("telegram answered %s %d times in a row: %w", wait.Type, unstated+1, err)
					}
					d = time.Second << unstated
					unstated++
				}
				waited, ok := budget.take(d, maxWait)
				if !ok {
					logWait(ctx, logger, wait, d, "telegram told a call to wait past the maximum; failing fast with a retry-after")
					return fmt.Errorf("telegram asked for a wait of %s, which with the %s already waited passes the %s maximum: %w", d, waited, maxWait, err)
				}
				logWait(ctx, logger, wait, d, "telegram told a call to wait; waiting it out")
				timer := time.NewTimer(d)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return fmt.Errorf("%w while waiting out the %s Telegram asked for (%w)", ctx.Err(), d, err)
				}
			}
		}
	})
}

// logWait logs one wait Telegram told a call to take: at Info for one chat's
// slow mode, a limit of that chat the model paces itself by, and at Warn for
// the account's and Telegram's own, which hold up every call alike.
func logWait(ctx context.Context, logger *slog.Logger, wait Wait, d time.Duration, msg string) {
	level := slog.LevelWarn
	if wait.Scope == ScopeChat {
		level = slog.LevelInfo
	}
	logger.Log(ctx, level, msg, "type", wait.Type, "wait_seconds", d.Seconds())
}
