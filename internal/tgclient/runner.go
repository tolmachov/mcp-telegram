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

const (
	// startClientTimeout bounds the connect + readiness handshake.
	startClientTimeout = 30 * time.Second
	// homeCheckTimeout bounds one home-DC check of a secondary DC's refusal
	// (see Running.checkHome).
	homeCheckTimeout = 20 * time.Second
)

var (
	// errClosed is why a client stopped when Close stopped it.
	errClosed = errors.New("telegram client closed")
	// errStoppedSilently is why a client stopped when gotd's Run loop ended
	// with no error of its own (gotd swallows cancellation).
	errStoppedSilently = errors.New("telegram client stopped without reporting an error")
)

// Running is a live Telegram client whose Run loop is owned by a background
// goroutine, so the caller can serve on it and stop it with Close. Both the
// stdio server and the per-user pool in HTTP mode run their clients this way.
type Running struct {
	api  *tg.Client
	self *tg.User

	// lifetime ends when the client stops; home-DC checks run on it.
	lifetime context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	logger   *slog.Logger
	// checks tracks the running home-DC check, so Close outlives it.
	checks sync.WaitGroup

	mu sync.Mutex
	// err is why the client stopped serving; nil while it serves.
	err error
	// connErr is the latest connection failure gotd reported, which is what
	// keeps a client that never becomes ready from doing so.
	connErr error
	// checking is set while a home-DC check runs, so the refusals it answers
	// start no other.
	checking bool
	// checkErr is why the last home-DC check got no answer, if it got none;
	// the refusals after it report it (see refusalWatch).
	checkErr error
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
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r := &Running{lifetime: runCtx, cancel: cancel, done: make(chan struct{}), logger: logger}
	base.Logger = gotdLogger(logger)
	base.OnDead = r.connDead
	base.Middlewares = []telegram.Middleware{r.refusalWatch()}
	client := newClient(cfg, storage, onFloodWait, base)
	r.api = client.API()

	ready := make(chan error, 1)
	go func() {
		defer close(r.done)
		err := client.Run(runCtx, func(ctx context.Context) error {
			self, err := client.Self(ctx)
			if err != nil {
				err = fmt.Errorf("checking the session: %w", err)
				ready <- err
				return err
			}
			r.self = self
			ready <- nil
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

	// readiness turns the callback's report into StartClient's answer. A
	// failed report goes through stop, so the verdict refusalWatch already
	// recorded — the home DC answers the readiness check's own call —
	// outranks the error the check then reports.
	readiness := func(err error) (*Running, error) {
		if err != nil {
			r.stop(sessionError(err))
			<-r.done
			return nil, r.Err()
		}
		return r, nil
	}
	select {
	case err := <-ready:
		return readiness(err)
	case <-r.done:
		select {
		case err := <-ready:
			return readiness(err)
		default:
		}
		// Run exited before the callback reported: a connect-phase failure.
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
// (floodWait), so it sees the reply to every try of a call, and it never waits
// on anything itself: a refused call returns at once.
//
// Which DC answered decides what a refusal (isSessionRefusal) means. gotd
// sends a call anywhere but the home DC only when the home DC answers it with
// FILE_MIGRATE or STATS_MIGRATE, which Telegram does for the upload.* file
// methods and the stats.* methods only (answeredAway). Any other call was
// answered by the home DC, whose refusal is the verdict: the client stops
// with ErrSessionUnauthorized and the call returns it. A refused call that
// may have gone to another DC returns ErrSecondaryRefusal at once and has
// the home DC checked in the background (checkHome), saying why the last
// check got no answer if it got none.
func (r *Running) refusalWatch() telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			err := next.Invoke(ctx, input, output)
			switch {
			case err == nil || !isSessionRefusal(err):
				return err //nolint:wrapcheck // a middleware passes Telegram's reply through unchanged.
			case !answeredAway(input):
				verdict := sessionError(err)
				r.stop(verdict)
				return verdict
			}
			checking, lastFailure := r.checkHome(next, err)
			switch {
			case !checking:
				// The client has stopped; why is its own answer.
				return err //nolint:wrapcheck // a middleware passes Telegram's reply through unchanged.
			case lastFailure != nil:
				// The check's failure is told, not wrapped: its timeout or
				// flood wait is not this call's, and must not classify it.
				return fmt.Errorf("%w (%w); the server is asking the home DC again whether the session still stands, as its last check got no answer (%v)", ErrSecondaryRefusal, err, lastFailure) //nolint:errorlint // see above.
			default:
				return fmt.Errorf("%w (%w); the server is asking the home DC whether the session still stands", ErrSecondaryRefusal, err)
			}
		}
	})
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

// checkHome starts asking the home DC, through next (the invoker below
// refusalWatch), whether it still accepts the session a secondary DC just
// refused, unless a check already runs, and reports whether one now runs —
// none does once the client has stopped — and why the last check got no
// answer, if it got none. The check runs on a goroutine of its
// own, bounded by homeCheckTimeout and the client's lifetime, and goes beneath
// the flood-wait middleware: a check Telegram tells to wait fails like any
// other that gets no answer. The home DC refusing the session is the verdict.
//
// The home DC accepting the session means the secondary DC never took the
// authorisation gotd exported to it. gotd keeps that DC's connection for the
// rest of the Run loop and exports the authorisation only to a connection it
// creates, so calls routed there keep being refused until the client
// reconnects: the check stops the client with a reason that says so, and its
// owner reconnects it. A check that gets no answer leaves the client serving
// and records why, so that the next refused call reports it and checks again.
func (r *Running) checkHome(next tg.Invoker, refusal error) (checking bool, lastFailure error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return false, nil
	}
	if !r.checking {
		r.checking = true
		r.checks.Add(1)
		go r.askHome(next, refusal)
	}
	return true, r.checkErr
}

// askHome is the check checkHome starts.
func (r *Running) askHome(next tg.Invoker, refusal error) {
	defer r.checks.Done()
	defer func() {
		r.mu.Lock()
		r.checking = false
		r.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(r.lifetime, homeCheckTimeout)
	defer cancel()
	_, err := tg.NewClient(next).UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
	switch {
	case err == nil:
		r.stop(fmt.Errorf("%w (%w) while the home DC still accepts it; reconnecting restores the calls it serves", ErrSecondaryRefusal, refusal))
	case isSessionRefusal(err):
		r.stop(sessionError(err))
	case r.lifetime.Err() != nil:
		// The client is stopping anyway.
	default:
		r.mu.Lock()
		r.checkErr = err
		r.mu.Unlock()
		r.logger.Warn("A Telegram DC other than the home one refused the session and the home DC could not be asked about it; keeping the client", "refusal", refusal, "err", err)
	}
}

// stop records why the client stops serving — the first reason wins — and
// ends its Run loop.
func (r *Running) stop(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
	r.cancel()
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
// wrapping ErrSecondaryRefusal when the home DC accepts a session another DC
// refused (see refusalWatch), otherwise the error its Run loop ended with.
func (r *Running) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Close disconnects the client and waits for its Run loop and any home-DC
// check to exit. Why a client stopped on its own before Close stays available
// from Err.
func (r *Running) Close() {
	r.stop(errClosed)
	<-r.done
	r.checks.Wait()
}
