package tgclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"golang.org/x/sync/singleflight"
)

// sessionError turns a refusal that ended the client's Run loop — a reply of
// the home DC — into the ErrSessionUnauthorized verdict, keeping its cause.
func sessionError(err error) error {
	if isSessionRefusal(err) && !errors.Is(err, ErrSessionUnauthorized) {
		return fmt.Errorf("%w: %w", ErrSessionUnauthorized, err)
	}
	return err
}

const (
	// startClientTimeout bounds the connect + auth-status readiness handshake.
	startClientTimeout = 30 * time.Second
	// refusalCheckTimeout bounds one home-DC check of a refused call (see
	// Running.confirmRefusal).
	refusalCheckTimeout = 20 * time.Second
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

	// lifetime ends when the client stops; refusal checks run on it.
	lifetime context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	logger   *slog.Logger
	// checks collapses the refusal checks of concurrently refused calls
	// into one.
	checks singleflight.Group

	mu sync.Mutex
	// err is why the client stopped serving; nil while it serves.
	err error
	// connErr is the latest connection failure gotd reported, which is what
	// keeps a client that never becomes ready from doing so.
	connErr error
}

// StartClient connects a Telegram client on top of the given session storage
// and blocks until it is ready to serve API calls (or fails). ctx gates only
// the startup handshake; the running client detaches and lives until Close,
// or until it stops on its own (see Err).
//
// The error is ErrSessionUnauthorized when Telegram does not accept the
// session — whether the auth check says so or gotd ends Run on a 401 before
// the check runs — and otherwise the failure that kept the client from
// finding out. gotd logs through logger at Warn and above.
func StartClient(ctx context.Context, cfg *Config, storage session.Storage, logger *slog.Logger, onFloodWait FloodWaitCallback) (*Running, error) {
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r := &Running{lifetime: runCtx, cancel: cancel, done: make(chan struct{}), logger: logger}
	client, run := newClient(cfg, storage, onFloodWait, telegram.Options{
		Logger:      gotdLogger(logger),
		OnDead:      r.connDead,
		Middlewares: []telegram.Middleware{refusalWatch(r.confirmRefusal)},
	})
	r.api = client.API()

	ready := make(chan error, 1)
	go func() {
		defer close(r.done)
		err := run(runCtx, func(ctx context.Context) error {
			status, err := client.Auth().Status(ctx)
			if err != nil {
				err = fmt.Errorf("checking Telegram auth status: %w", err)
				ready <- err
				return err
			}
			if !status.Authorized {
				ready <- ErrSessionUnauthorized
				return ErrSessionUnauthorized
			}
			r.self = status.User
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
	// failed report goes through stop, so a refusal already confirmed through
	// refusalWatch — the auth check's own call can be the one Telegram
	// refused — outranks the bare verdict the check then reports.
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

// confirmRefusal decides whether refusal, Telegram's reply to one call, means
// the session is dead (see refusalWatch). A single reply is not a verdict: a
// DC other than the home one — where FILE_MIGRATE sends a download — refuses a
// session whose exported authorisation it never took, while the home DC still
// accepts it. So the home DC is asked, through next (the invoker below
// refusalWatch, so the check is not itself watched), who the session belongs
// to. The check is shared by every call refused while it runs and belongs to
// none of them: it runs on the client's lifetime bounded by
// refusalCheckTimeout, and each caller waits on its own ctx.
//
// A confirmed refusal stops the client and reaches the caller as
// ErrSessionUnauthorized wrapping refusal; otherwise the caller gets refusal
// unchanged and the client keeps serving.
func (r *Running) confirmRefusal(ctx context.Context, next tg.Invoker, refusal error) error {
	check := r.checks.DoChan("session", func() (any, error) { return r.checkSession(next, refusal), nil })
	select {
	case <-ctx.Done():
		return refusal
	case outcome := <-check:
		if outcome.Val.(bool) {
			return fmt.Errorf("%w: %w", ErrSessionUnauthorized, refusal)
		}
		return refusal
	}
}

// checkSession asks the home DC whether it still accepts the session, stops
// the client when it does not, and reports whether it did.
func (r *Running) checkSession(next tg.Invoker, refusal error) (dead bool) {
	ctx, cancel := context.WithTimeout(r.lifetime, refusalCheckTimeout)
	defer cancel()
	_, err := tg.NewClient(next).UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
	switch {
	case err == nil:
		r.logger.Warn("Telegram refused a call, but the home DC still accepts the session; keeping the client", "refusal", refusal)
		return false
	case isSessionRefusal(err):
		r.stop(sessionError(err))
		return true
	case r.lifetime.Err() != nil:
		// The client is stopping anyway; there is nothing left to decide.
		return false
	default:
		r.logger.Warn("Telegram refused a call and the home DC could not be asked about the session; keeping the client", "refusal", refusal, "err", err)
		return false
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
// wrapping ErrSessionUnauthorized as soon as the home DC confirms that
// Telegram refuses the session (see confirmRefusal), otherwise the error its
// Run loop ended with.
func (r *Running) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Close disconnects the client and waits for its Run loop to exit. Why a
// client stopped on its own before Close stays available from Err.
func (r *Running) Close() {
	r.stop(errClosed)
	<-r.done
}
