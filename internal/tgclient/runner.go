package tgclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// sessionError turns a refusal by the home DC — the error that ended the
// client's Run loop, failed its readiness check or answered one of its calls —
// into the ErrSessionUnauthorized verdict, keeping its cause.
func sessionError(err error) error {
	if isSessionRefusal(err) && !errors.Is(err, ErrSessionUnauthorized) {
		return fmt.Errorf("%w: %w", ErrSessionUnauthorized, err)
	}
	return err
}

// startClientTimeout bounds the connect + readiness handshake.
const startClientTimeout = 30 * time.Second

var (
	// errClosed is why a client stopped when Close stopped it.
	errClosed = errors.New("telegram client closed")
	// errStoppedSilently is why a client stopped when gotd's Run loop ended
	// with no error of its own (gotd swallows cancellation).
	errStoppedSilently = errors.New("telegram client stopped without reporting an error")
	// errAwayRefusal is why a client stopped when a DC other than the home
	// one refused the session (see Running.refusalWatch).
	errAwayRefusal = errors.New("a Telegram DC other than the account's home one refused the session")
)

// Running is a live Telegram client whose Run loop is owned by a background
// goroutine, so the caller can serve on it and stop it with Close. Both the
// stdio server and the per-user pool in HTTP mode run their clients this way.
type Running struct {
	api  *tg.Client
	self *tg.User

	// lifetime ends when the client stops, with why as its cause: stop ends
	// it, and the first reason wins.
	lifetime context.Context
	stop     context.CancelCauseFunc
	done     chan struct{}

	mu sync.Mutex
	// connErr is the latest connection failure gotd reported, which is what
	// keeps a client that never becomes ready from doing so.
	connErr error
}

// StartClient connects a Telegram client on top of the given session storage
// and blocks until it is ready to serve API calls (or fails). ctx gates only
// the startup handshake; the running client detaches and lives until Close,
// or until it stops on its own (see Err).
//
// The error is ErrSessionUnauthorized, wrapping the home DC's reply, when
// Telegram does not accept the session — whether the readiness check is
// refused or gotd ends Run on a 401 before the check runs — and otherwise the
// failure that kept the client from finding out. gotd logs through logger at
// Warn and above.
func StartClient(ctx context.Context, cfg *Config, storage session.Storage, logger *slog.Logger, onFloodWait FloodWaitCallback) (*Running, error) {
	return startClient(ctx, cfg, storage, logger, onFloodWait, telegram.Options{})
}

// startClient is StartClient over base, the gotd options that say which
// Telegram servers to reach: the defaults in production, an in-process
// cluster in tests. Everything else the client needs is set here.
func startClient(ctx context.Context, cfg *Config, storage session.Storage, logger *slog.Logger, onFloodWait FloodWaitCallback, base telegram.Options) (*Running, error) {
	lifetime, stop := context.WithCancelCause(context.WithoutCancel(ctx))
	r := &Running{lifetime: lifetime, stop: stop, done: make(chan struct{})}
	base.Logger = gotdLogger(logger)
	base.OnDead = r.connDead
	base.Middlewares = []telegram.Middleware{r.refusalWatch()}
	client := newClient(cfg, storage, onFloodWait, base)
	r.api = tg.NewClient(r.stopWatch(client))

	// ready closes once the readiness check has passed. A check that fails
	// ends the Run loop with its error, which goes through stop like any
	// other, so the verdict refusalWatch already recorded — the home DC
	// answers the check's own call — outranks it.
	ready := make(chan struct{})
	go func() {
		defer close(r.done)
		err := client.Run(lifetime, func(ctx context.Context) error {
			self, err := client.Self(ctx)
			if err != nil {
				return fmt.Errorf("checking the session: %w", err)
			}
			r.self = self
			close(ready)
			// Stay connected until Close (or a fatal client error) ends
			// the Run loop. Returning nil here would disconnect.
			<-ctx.Done()
			return nil
		})
		if err == nil {
			err = errStoppedSilently
		}
		r.stop(sessionError(err))
	}()

	select {
	case <-ready:
		return r, nil
	case <-r.done:
		return nil, r.Err()
	case <-time.After(startClientTimeout):
		r.stop(r.startTimeoutError())
		<-r.done
		return nil, r.Err()
	case <-ctx.Done():
		r.stop(ctx.Err())
		<-r.done
		return nil, ctx.Err()
	}
}

// startTimeoutError names why a client missed startClientTimeout: gotd keeps
// retrying a failing connection inside Run, so the last failure it reported is
// the cause, and a timeout without one means the handshake itself stalled.
func (r *Running) startTimeoutError() error {
	r.mu.Lock()
	cause := r.connErr
	r.mu.Unlock()
	if cause == nil {
		return fmt.Errorf("telegram client did not become ready within %s; no connection failure was reported, so the handshake stalled", startClientTimeout)
	}
	return fmt.Errorf("telegram client did not become ready within %s; last connection failure: %w", startClientTimeout, cause)
}

// connDead records a connection failure gotd reported (telegram.Options.OnDead).
func (r *Running) connDead(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connErr = err
}

// refusalWatch is the one place a client learns that Telegram refused its
// session: every call's reply passes through it, whichever tool, resource or
// background poller made the call. It sits inside the flood-wait middleware
// (floodWait), so it sees the reply to every try of a call.
//
// Which DC answered decides what a refusal (isSessionRefusal) means. gotd
// sends a call anywhere but the home DC only when the home DC answers it with
// FILE_MIGRATE or STATS_MIGRATE, which Telegram does for the upload.* file
// methods and the stats.* methods only (answeredAway). Any other call was
// answered by the home DC, whose refusal is the verdict: the client stops
// with ErrSessionUnauthorized. A refusal by another DC says only that it did
// not take the authorisation gotd exported to it, and gotd keeps that DC's
// connection for the rest of the Run loop, exporting the authorisation only
// to a connection it creates: the client stops so that its owner reconnects
// it, and the readiness check of the reconnect has the home DC judge the
// session. Either way the call itself fails as every call does once the
// client has stopped (see stopWatch).
func (r *Running) refusalWatch() telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			err := next.Invoke(ctx, input, output)
			switch {
			case err == nil || !isSessionRefusal(err):
			case answeredAway(input):
				r.stop(fmt.Errorf("%w (%w); reconnecting has the home DC judge the session", errAwayRefusal, err))
			default:
				r.stop(sessionError(err))
			}
			return err //nolint:wrapcheck // a middleware passes Telegram's reply through unchanged.
		}
	})
}

// stopWatch is the invoker the client's API calls through, outside every
// middleware: once the client has stopped, a call that fails fails with
// ErrClientStopped wrapping why (Err), instead of the cancelled context or
// closed connection gotd's shutdown leaves it with — which say nothing of the
// cause, and would pass for the caller's own cancellation.
func (r *Running) stopWatch(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		err := next.Invoke(ctx, input, output)
		if err != nil && r.lifetime.Err() != nil {
			return Stopped(r.Err())
		}
		return err //nolint:wrapcheck // the client passes Telegram's reply through unchanged.
	}
}

// awayMethods holds the TL type IDs of the methods gotd may send to a DC
// other than the home one: the upload.* and stats.* namespaces, the ones
// Telegram answers with FILE_MIGRATE or STATS_MIGRATE (see refusalWatch).
var awayMethods = sync.OnceValue(func() map[uint32]bool {
	ids := make(map[uint32]bool)
	for id, name := range tg.TypesMap() {
		if strings.HasPrefix(name, "upload.") || strings.HasPrefix(name, "stats.") {
			ids[id] = true
		}
	}
	return ids
})

// answeredAway reports whether input is a call gotd may have sent to a DC
// other than the home one.
func answeredAway(input bin.Encoder) bool {
	method, ok := input.(interface{ TypeID() uint32 })
	return ok && awayMethods()[method.TypeID()]
}

// API returns the RPC client for tool handlers.
func (r *Running) API() *tg.Client { return r.api }

// Self returns the account the session is authorized as, from the readiness
// check.
func (r *Running) Self() *tg.User { return r.self }

// Done is closed once the Run loop has exited, whether through Close or on
// its own (see Err).
func (r *Running) Done() <-chan struct{} { return r.done }

// Err returns nil while the client serves and, from the moment it stops, why:
// wrapping ErrSessionUnauthorized as soon as the home DC refuses the session,
// otherwise the reason it stopped for — another DC refusing the session (see
// refusalWatch), Close, or the error its Run loop ended with.
func (r *Running) Err() error { return context.Cause(r.lifetime) } //nolint:wrapcheck // the reason stop recorded, as is.

// Close disconnects the client and waits for its Run loop to exit. Why a
// client stopped on its own before Close stays available from Err.
func (r *Running) Close() {
	r.stop(errClosed)
	<-r.done
}
