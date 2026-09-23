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
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"golang.org/x/sync/singleflight"
)

// refusedByHome reports whether err, a reply of the home DC, declares the
// session dead. The home DC owns the session, so any 401 it answers is the
// verdict, the ones isSessionRefusal leaves out for other DCs included: a
// session still waiting for its 2FA password cannot serve either.
func refusedByHome(err error) bool {
	return isSessionRefusal(err) || auth.IsUnauthorized(err)
}

// sessionError turns a refusal by the home DC — the error that ended the
// client's Run loop or failed its readiness check — into the
// ErrSessionUnauthorized verdict, keeping its cause.
func sessionError(err error) error {
	if refusedByHome(err) && !errors.Is(err, ErrSessionUnauthorized) {
		return fmt.Errorf("%w: %w", ErrSessionUnauthorized, err)
	}
	return err
}

const (
	// startClientTimeout bounds the connect + readiness handshake.
	startClientTimeout = 30 * time.Second
	// refusalCheckTimeout bounds one home-DC check of a refused call (see
	// Running.confirmRefusal).
	refusalCheckTimeout = 20 * time.Second
	// homeAcceptedTTL is how long the home DC accepting the session answers
	// later refusals without asking it again, so a batch of calls to a DC
	// that refuses the session costs one check, not one per call.
	homeAcceptedTTL = time.Minute
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
	now      func() time.Time
	// checks collapses the refusal checks of concurrently refused calls
	// into one.
	checks singleflight.Group

	mu sync.Mutex
	// err is why the client stopped serving; nil while it serves.
	err error
	// connErr is the latest connection failure gotd reported, which is what
	// keeps a client that never becomes ready from doing so.
	connErr error
	// homeAccepted is when the home DC last accepted the session on a
	// refusal check; zero before any.
	homeAccepted time.Time
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
	r := &Running{lifetime: runCtx, cancel: cancel, done: make(chan struct{}), logger: logger, now: time.Now}
	base.Logger = gotdLogger(logger)
	base.OnDead = r.connDead
	base.Middlewares = []telegram.Middleware{refusalWatch(r.confirmRefusal)}
	client, run := newClient(cfg, storage, onFloodWait, base)
	r.api = client.API()

	ready := make(chan error, 1)
	go func() {
		defer close(r.done)
		err := run(runCtx, func(ctx context.Context) error {
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
	// failed report goes through stop, so a refusal already confirmed through
	// refusalWatch — the check's own call can be the one Telegram refused —
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

// confirmRefusal decides whether refusal, Telegram's reply to one call, means
// the session is dead (see refusalWatch). A single reply is not a verdict: a
// DC other than the home one — where FILE_MIGRATE sends a download — refuses a
// session whose exported authorisation it never took, while the home DC still
// accepts it. So the home DC is asked, through next (the invoker below
// refusalWatch, so the check is not itself watched), who the session belongs
// to. The check is shared by every call refused while it runs and belongs to
// none of them: it runs on the client's lifetime bounded by
// refusalCheckTimeout, and each caller waits on its own ctx. A home DC that
// accepted the session within homeAcceptedTTL is not asked again.
//
// A confirmed refusal stops the client and reaches the caller as
// ErrSessionUnauthorized wrapping refusal. An unconfirmed one leaves the
// client serving and reaches the caller as an *UnconfirmedRefusalError. A
// caller that stops waiting gets refusal together with its ctx's error.
func (r *Running) confirmRefusal(ctx context.Context, next tg.Invoker, refusal error) error {
	if stopped := r.Err(); stopped != nil {
		return refusalOutcome(refusal, stopped)
	}
	if r.homeRecentlyAccepted() {
		return &UnconfirmedRefusalError{Refusal: refusal}
	}
	check := r.checks.DoChan("session", func() (any, error) { return nil, r.checkSession(next, refusal) })
	select {
	case <-ctx.Done():
		return fmt.Errorf("%w; stopped waiting for the home DC to check the session: %w", refusal, ctx.Err())
	case outcome := <-check:
		return refusalOutcome(refusal, outcome.Err)
	}
}

// refusalOutcome is what a call Telegram answered with refusal returns, given
// check, the home DC's answer about the session (see checkSession).
func refusalOutcome(refusal, check error) error {
	switch {
	case check == nil:
		return &UnconfirmedRefusalError{Refusal: refusal}
	case errors.Is(check, ErrSessionUnauthorized):
		return fmt.Errorf("%w: %w", ErrSessionUnauthorized, refusal)
	default:
		return &UnconfirmedRefusalError{Refusal: refusal, Check: check}
	}
}

// homeRecentlyAccepted reports whether the home DC accepted the session within
// homeAcceptedTTL.
func (r *Running) homeRecentlyAccepted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.homeAccepted.IsZero() && r.now().Sub(r.homeAccepted) < homeAcceptedTTL
}

// checkSession asks the home DC whether it still accepts the session and
// stops the client when it does not. It returns nil when the home DC accepts
// the session, why the client stopped when it has — the verdict among the
// reasons — and otherwise why the home DC could not be asked.
func (r *Running) checkSession(next tg.Invoker, refusal error) error {
	ctx, cancel := context.WithTimeout(r.lifetime, refusalCheckTimeout)
	defer cancel()
	_, err := tg.NewClient(next).UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
	switch {
	case err == nil:
		r.mu.Lock()
		r.homeAccepted = r.now()
		r.mu.Unlock()
		r.logger.Warn("Telegram refused a call, but the home DC still accepts the session; keeping the client", "refusal", refusal)
		return nil
	case refusedByHome(err):
		r.stop(sessionError(err))
		return r.Err()
	case r.lifetime.Err() != nil:
		// The client is stopping anyway; why it stopped answers the call.
		return r.Err()
	default:
		r.logger.Warn("Telegram refused a call and the home DC could not be asked about the session; keeping the client", "refusal", refusal, "err", err)
		return fmt.Errorf("asking the home DC: %w", err)
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
