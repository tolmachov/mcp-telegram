package tgclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// ErrSessionUnauthorized means the stored session exists but Telegram no
// longer accepts it (never logged in, logged out remotely, or the auth key
// was revoked). The HTTP layer maps this to 401 so the client re-runs the
// OAuth + QR login flow.
var ErrSessionUnauthorized = errors.New("telegram session is not authorized")

// startClientTimeout bounds the connect + auth-status readiness handshake.
const startClientTimeout = 30 * time.Second

// Running is a live Telegram client whose Run loop is owned by a background
// goroutine — the inversion of the stdio mode, where client.Run wraps the
// serve loop. Used by the per-user pool in HTTP mode.
type Running struct {
	client *telegram.Client

	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	runErr error
}

// StartClient connects a Telegram client on top of the given session storage
// and blocks until it is ready to serve API calls (or fails). ctx gates only
// the startup handshake; the running client detaches and lives until Close.
//
// The not-logged-in condition is reported as ErrSessionUnauthorized so
// callers can distinguish "user must re-login" from transport failures.
func StartClient(ctx context.Context, cfg *Config, storage session.Storage, onFloodWait FloodWaitCallback) (*Running, error) {
	waiter := floodwait.NewWaiter().WithMaxWait(cfg.EffectiveFloodWaitMaxWait())
	if onFloodWait != nil {
		waiter = waiter.WithCallback(func(ctx context.Context, wait floodwait.FloodWait) {
			onFloodWait(ctx, wait.Duration)
		})
	}
	client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: storage,
		Middlewares:    []telegram.Middleware{waiter},
	})

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r := &Running{
		client: client,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	ready := make(chan error, 1)
	go func() {
		defer close(r.done)
		err := waiter.Run(runCtx, func(ctx context.Context) error {
			return client.Run(ctx, func(ctx context.Context) error {
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
				ready <- nil
				// Stay connected until Close (or a fatal client error) ends
				// the Run loop. Returning nil here would disconnect.
				<-ctx.Done()
				return nil
			})
		})
		r.setRunErr(err)
		cancel()
	}()

	select {
	case err := <-ready:
		if err != nil {
			cancel()
			<-r.done
			return nil, err
		}
		return r, nil
	case <-r.done:
		// Run exited before reporting readiness: connect/dial failure.
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
func (r *Running) API() *tg.Client { return r.client.API() }

// Client returns the underlying gotd client (completion handler needs it).
func (r *Running) Client() *telegram.Client { return r.client }

// RunErr returns the error the Run loop exited with, once it has.
func (r *Running) RunErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runErr
}

func (r *Running) setRunErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runErr = err
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
