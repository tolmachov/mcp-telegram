package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// accountClient is a fakeClient whose API answers every call with its own
// name, so a test can tell which client served a call.
func accountClient(name string) *fakeClient {
	c := newFakeClient()
	c.api = tg.NewClient(namedInvoker(name))
	return c
}

// namedInvoker fails every call with its own name.
type namedInvoker string

func (n namedInvoker) Invoke(context.Context, bin.Encoder, bin.Decoder) error {
	return errors.New(string(n))
}

// servedBy returns which accountClient served a call through c.
func servedBy(c *reconnectingClient) string {
	_, err := c.API().HelpGetConfig(context.Background())
	return err.Error()
}

// connects is a scripted connectLocal: each call returns the next of its
// answers and counts itself.
type connects struct {
	mu      sync.Mutex
	answers []func() (localClient, error)
	calls   int
}

func (c *connects) connect(context.Context) (localClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if len(c.answers) == 0 {
		return nil, errors.New("unscripted connect")
	}
	next := c.answers[0]
	c.answers = c.answers[1:]
	return next()
}

func (c *connects) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func connected(client localClient) func() (localClient, error) {
	return func() (localClient, error) { return client, nil }
}

func failedConnect(err error) func() (localClient, error) {
	return func() (localClient, error) { return nil, err }
}

// TestReconnectingClientReplacesAStoppedClient pins the stdio reconnect: a
// client that stops for any reason but a refused session is replaced, calls
// go to the new one, and the old one is disconnected. While it is being
// replaced, Err says why it stopped; failed reconnects back off, doubling
// from a second.
func TestReconnectingClientReplacesAStoppedClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := accountClient("first"), accountClient("second")
		unreachable := errors.New("dial tcp: i/o timeout")
		script := &connects{answers: []func() (localClient, error){failedConnect(unreachable), failedConnect(unreachable), connected(second)}}
		c := newReconnectingClient(t.Context(), first, script.connect, testLogger())
		defer c.Close()

		require.NoError(t, c.Err())
		assert.Equal(t, "first", servedBy(c))

		dropped := errors.New("read tcp: connection reset by peer")
		start := time.Now()
		first.stop(dropped)
		synctest.Wait()
		require.ErrorIs(t, c.Err(), dropped, "the calls meanwhile are told why")
		assert.True(t, first.isClosed(), "the stopped client is disconnected")
		assert.Equal(t, 1, script.count(), "the first reconnect is at once")

		time.Sleep(3 * time.Second)
		synctest.Wait()
		assert.Equal(t, 3, script.count(), "failed reconnects back off a second, then two")
		assert.Equal(t, 3*time.Second, time.Since(start))
		require.NoError(t, c.Err())
		assert.Equal(t, "second", servedBy(c))
		select {
		case <-c.Done():
			t.Fatal("a reconnected client has not stopped")
		default:
		}
	})
}

// TestReconnectingClientStopsOnARefusedSession pins the end of the
// reconnects: a session Telegram refuses — when a client stops on it, or a
// reconnect's readiness check is refused — stops the client for good, with
// the refusal as why, the login-required state reached mid-session.
func TestReconnectingClientStopsOnARefusedSession(t *testing.T) {
	refused := fmt.Errorf("%w: %w", tgclient.ErrSessionUnauthorized, tgerr.New(401, "SESSION_REVOKED"))
	t.Run("the running client is refused", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			first := accountClient("first")
			script := &connects{}
			c := newReconnectingClient(t.Context(), first, script.connect, testLogger())
			defer c.Close()

			first.stop(refused)
			<-c.Done()
			require.ErrorIs(t, c.Err(), tgclient.ErrSessionUnauthorized)
			assert.Zero(t, script.count(), "a refused session is not reconnected")
			assert.True(t, first.isClosed())
		})
	})
	t.Run("the reconnect is refused", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			first := accountClient("first")
			script := &connects{answers: []func() (localClient, error){failedConnect(fmt.Errorf("starting Telegram client: %w", refused))}}
			c := newReconnectingClient(t.Context(), first, script.connect, testLogger())
			defer c.Close()

			first.stop(errors.New("a Telegram DC other than the account's home one refused the session"))
			<-c.Done()
			require.ErrorIs(t, c.Err(), tgclient.ErrSessionUnauthorized, "the reconnect's readiness check is the verdict")
			assert.Equal(t, 1, script.count())
		})
	})
}

// TestReconnectingClientCloseEndsAReconnect pins that Close does not wait
// out a reconnect's back-off, and disconnects the client it had.
func TestReconnectingClientCloseEndsAReconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first := accountClient("first")
		script := &connects{answers: []func() (localClient, error){failedConnect(errors.New("dial tcp: i/o timeout"))}}
		c := newReconnectingClient(t.Context(), first, script.connect, testLogger())

		first.stop(errors.New("read tcp: connection reset by peer"))
		synctest.Wait()
		start := time.Now()
		c.Close()
		assert.Zero(t, time.Since(start), "Close does not wait out the back-off")
		assert.Equal(t, 1, script.count())
		select {
		case <-c.Done():
		default:
			t.Fatal("a closed client has stopped")
		}

		serving := accountClient("serving")
		c = newReconnectingClient(t.Context(), serving, script.connect, testLogger())
		c.Close()
		assert.True(t, serving.isClosed(), "Close disconnects the client it has")
	})
}
