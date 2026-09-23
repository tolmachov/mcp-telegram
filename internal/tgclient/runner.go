package tgclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/tg"
)

func sessionError(err error) error {
	if IsSessionUnauthorized(err) && !errors.Is(err, ErrSessionUnauthorized) {
		return fmt.Errorf("%w: %w", ErrSessionUnauthorized, err)
	}
	return err
}

// startClientTimeout bounds the connect + auth-status readiness handshake.
const startClientTimeout = 30 * time.Second

// Running is a live Telegram client whose Run loop is owned by a background
// goroutine, so the caller can serve on it and stop it with Close. Both the
// stdio server and the per-user pool in HTTP mode run their clients this way.
type Running struct {
	api  *tg.Client
	self *tg.User

	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	runErr error
}

// StartClient connects a Telegram client on top of the given session storage
// and blocks until it is ready to serve API calls (or fails). ctx gates only
// the startup handshake; the running client detaches and lives until Close.
//
// It is the one answer to "can this session serve": nil with a ready client,
// ErrSessionUnauthorized when Telegram does not accept the session, or the
// failure that kept it from finding out. That covers the failures gotd
// reports without ever running the ready callback — restoreConnection's
// "corrupted key" returns before the callback starts, and a connect-phase 401
// (AUTH_KEY_UNREGISTERED, SESSION_EXPIRED, …) ends Run from a sibling
// goroutine — and a Run that ends with a nil error without having reported
// (gotd swallows cancellation), which is never mistaken for a verdict. A
// session revoked from Telegram's Devices list often surfaces the friendlier
// way instead — auth.Status maps a 401 on users.getUsers to Status{} with no
// error — and both routes land on ErrSessionUnauthorized.
func StartClient(ctx context.Context, cfg *Config, storage session.Storage, onFloodWait FloodWaitCallback) (*Running, error) {
	client, run := newClient(cfg, storage, onFloodWait)

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r := &Running{
		api:    client.API(),
		cancel: cancel,
		done:   make(chan struct{}),
	}
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
		r.setRunErr(err)
		cancel()
	}()

	// readiness turns the callback's report into StartClient's answer. A
	// report outranks however Run itself ended: once the auth check has
	// answered, a later error can only come from Run's teardown.
	readiness := func(err error) (*Running, error) {
		if err != nil {
			cancel()
			<-r.done
			return nil, sessionError(err)
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
		err := r.RunErr()
		if err == nil {
			err = errors.New("telegram client exited during startup")
		}
		return nil, err
	case <-time.After(startClientTimeout):
		cancel()
		<-r.done
		return nil, fmt.Errorf("telegram client did not become ready within %s", startClientTimeout)
	case <-ctx.Done():
		cancel()
		<-r.done
		return nil, ctx.Err()
	}
}

// API returns the RPC client for tool handlers.
func (r *Running) API() *tg.Client { return r.api }

// Self returns the account the session is authorized as, from the readiness
// check.
func (r *Running) Self() *tg.User { return r.self }

// Done is closed once the Run loop has exited, whether through Close or a
// fatal client error (see RunErr).
func (r *Running) Done() <-chan struct{} { return r.done }

// RunErr returns the error the Run loop exited with, once it has.
func (r *Running) RunErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runErr
}

func (r *Running) setRunErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runErr = sessionError(err)
}

// Close disconnects the client and waits for the Run loop to exit. A
// context.Canceled exit is the normal teardown path and is not an error.
func (r *Running) Close() error {
	r.cancel()
	<-r.done
	if err := r.RunErr(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
