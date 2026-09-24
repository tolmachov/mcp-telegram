package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// reconnectMaxDelay caps the back-off between failed reconnects.
const reconnectMaxDelay = time.Minute

// reconnectingClient is the single local account's Telegram client over
// stdio: the client connectLocal connected, connected again whenever it stops
// for any reason but Telegram refusing the session, as the HTTP pool rebuilds
// a user's assembly on their next request. Over stdio the assembly cannot be
// rebuilt instead — its MCP session is the host's, which the SDK's stdio
// connection cannot hand to another (see serveAssembly) — so it is built
// once, over API, whose calls go to whichever client is current, and
// outlives the reconnects.
//
// A session Telegram refuses, whether a client stops on it or a reconnect's
// readiness check is refused, ends the reconnects: Done closes and Err keeps
// the refusal, the login-required state reached mid-session (see
// clientDownMiddleware). Until then Err is nil while a client serves and,
// while the one that stopped is being replaced, why it stopped.
type reconnectingClient struct {
	api     *tg.Client
	connect func(context.Context) (localClient, error)
	logger  *slog.Logger
	// life ends on Close; reconnects run on it.
	life context.Context
	end  context.CancelFunc
	// done closes once the reconnect loop has returned: the client has
	// stopped for good.
	done chan struct{}

	mu      sync.Mutex
	current localClient
	// refused is why a reconnect found the session refused.
	refused error
}

// newReconnectingClient serves on first, reconnecting through connect, until
// ctx ends or Close.
func newReconnectingClient(ctx context.Context, first localClient, connect func(context.Context) (localClient, error), logger *slog.Logger) *reconnectingClient {
	life, end := context.WithCancel(ctx)
	c := &reconnectingClient{connect: connect, logger: logger, life: life, end: end, done: make(chan struct{}), current: first}
	c.api = tg.NewClient(c)
	go c.run()
	return c
}

// Invoke sends a call through the current client. Once that client has
// stopped, the call fails with why (tgclient.ErrClientStopped) until a
// reconnect replaces it.
func (c *reconnectingClient) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	return c.client().API().Invoker().Invoke(ctx, input, output) //nolint:wrapcheck // the current client's reply, passed through unchanged.
}

func (c *reconnectingClient) client() localClient {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *reconnectingClient) API() *tg.Client       { return c.api }
func (c *reconnectingClient) Done() <-chan struct{} { return c.done }

func (c *reconnectingClient) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refused != nil {
		return c.refused
	}
	return c.current.Err()
}

// Close stops reconnecting and disconnects the current client.
func (c *reconnectingClient) Close() {
	c.end()
	<-c.done
}

// run replaces each client that stops until the session is refused or the
// client is closed, and disconnects the last one.
func (c *reconnectingClient) run() {
	defer close(c.done)
	for {
		current := c.client()
		select {
		case <-current.Done():
		case <-c.life.Done():
		}
		current.Close()
		reason := current.Err()
		if c.life.Err() != nil || errors.Is(reason, tgclient.ErrSessionUnauthorized) {
			return
		}
		c.logger.Warn("Telegram client stopped; reconnecting", "reason", reason)
		next, err := c.reconnect()
		if err != nil {
			if c.life.Err() == nil {
				c.logger.Warn("Telegram refused the session on reconnecting; tool calls and resource reads now answer with the reason", "err", err)
				c.mu.Lock()
				c.refused = err
				c.mu.Unlock()
			}
			return
		}
		c.logger.Info("reconnected to Telegram")
		c.mu.Lock()
		c.current = next
		c.mu.Unlock()
	}
}

// reconnect connects a new client, retrying a failure with a back-off that
// doubles from a second up to reconnectMaxDelay. It gives up when the session
// is refused or the client is closed.
func (c *reconnectingClient) reconnect() (localClient, error) {
	var delay time.Duration
	for {
		next, err := c.connect(c.life)
		if err == nil || errors.Is(err, tgclient.ErrSessionUnauthorized) || c.life.Err() != nil {
			return next, err
		}
		delay = min(max(2*delay, time.Second), reconnectMaxDelay)
		c.logger.Warn("reconnecting to Telegram failed; retrying", "err", err, "retry_in", delay)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-c.life.Done():
			timer.Stop()
			return nil, c.life.Err()
		}
	}
}
