package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	telegramfake "github.com/tolmachov/mcp-telegram/internal/testutil/telegram"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tools"
)

// TestClientDownMiddlewarePassesOtherMethods pins what the middleware leaves
// alone once the client has stopped: methods other than tool calls and
// resource reads — completion, which answers the user's input box, and the
// tool list — reach their handler, and a resource read that succeeded while
// the client stopped keeps what it read.
func TestClientDownMiddlewarePassesOtherMethods(t *testing.T) {
	refused := fmt.Errorf("%w: %w", tgclient.ErrSessionUnauthorized, tgerr.New(401, "SESSION_REVOKED"))
	srv := &Server{opts: Options{Transport: TransportStdio}}

	stopped := newFakeClient()
	stopped.stop(refused)
	for method, want := range map[string]mcp.Result{
		"completion/complete": &mcp.CompleteResult{},
		methodListTools:       &mcp.ListToolsResult{},
	} {
		handler := srv.clientDownMiddleware(stopped)(func(_ context.Context, got string, _ mcp.Request) (mcp.Result, error) {
			assert.Equal(t, method, got)
			return want, nil
		})
		res, err := handler(t.Context(), method, nil)
		require.NoError(t, err, method)
		assert.Same(t, want, res, "%s reaches its handler", method)
	}

	stopping := newFakeClient()
	read := &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "telegram://me", Text: "{}"}}}
	handler := srv.clientDownMiddleware(stopping)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		stopping.stop(refused)
		return read, nil
	})
	res, err := handler(t.Context(), methodReadResource, &mcp.ReadResourceRequest{})
	require.NoError(t, err)
	assert.Same(t, read, res, "what a read got before the stop is still true")
}

// callThroughClientDown registers handler on a server behind the
// clientDownMiddleware of tgClient and calls its tool name with args,
// returning the text blocks of the result.
func callThroughClientDown(t *testing.T, srv *Server, tgClient telegramClient, handler tools.Handler, name string, args map[string]any) (texts []string, isError bool) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	server.AddReceivingMiddleware(srv.clientDownMiddleware(tgClient))
	handler.Register(server)

	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), serverT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(t.Context(), clientT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	for _, c := range res.Content {
		texts = append(texts, c.(*mcp.TextContent).Text)
	}
	return texts, res.IsError
}

// TestClientDownExplainsADeadSessionOnce pins who explains a dead session: a
// real tool whose call the home DC refused renders only its failure, and the
// server appends the transport's explanation — so the model reads it once.
func TestClientDownExplainsADeadSessionOnce(t *testing.T) {
	dead := fmt.Errorf("%w: %w", tgclient.ErrSessionUnauthorized, tgerr.New(401, "SESSION_REVOKED"))
	for _, transport := range []string{TransportStdio, TransportHTTP} {
		t.Run(transport, func(t *testing.T) {
			srv := &Server{opts: Options{Transport: transport}}
			tgClient := newFakeClient()
			// The client stops before the refused call returns, as
			// tgclient.Running does on a confirmed refusal.
			api := tg.NewClient(telegramfake.New(telegramfake.Typed(func(context.Context, *tg.UsersGetFullUserRequest, *tg.UsersUserFull) error {
				tgClient.stop(dead)
				return dead
			})))

			texts, isError := callThroughClientDown(t, srv, tgClient, tools.NewMeGetHandler(api), "GetMe", map[string]any{})
			require.True(t, isError)
			require.Len(t, texts, 2)
			assert.Equal(t, "Failed to get current user: getting current user: telegram session is not authorized: rpc error code 401: SESSION_REVOKED.",
				texts[0], "the tool renders only its failure")
			assert.Equal(t, srv.clientDownText(dead), texts[1], "the server explains the dead session")
		})
	}
}

// TestClientDownAgreesWithAnAwayRefusal pins that a call a DC other than the
// home one refused, which stops the client for a reconnect, is told one
// thing: the connection is being restored and the call can be retried —
// never to log out or sign in.
func TestClientDownAgreesWithAnAwayRefusal(t *testing.T) {
	away := errors.New("a Telegram DC other than the account's home one refused the session (rpc error code 401: AUTH_KEY_UNREGISTERED); reconnecting has the home DC judge the session")
	for _, transport := range []string{TransportStdio, TransportHTTP} {
		t.Run(transport, func(t *testing.T) {
			srv := &Server{opts: Options{Transport: transport}}
			tgClient := newFakeClient()
			api := tg.NewClient(telegramfake.New(telegramfake.Typed(func(context.Context, *tg.UsersGetFullUserRequest, *tg.UsersUserFull) error {
				tgClient.stop(away)
				return tgclient.Stopped(away)
			})))

			texts, isError := callThroughClientDown(t, srv, tgClient, tools.NewMeGetHandler(api), "GetMe", map[string]any{})
			require.True(t, isError)
			require.Len(t, texts, 2)
			assert.Equal(t, "Failed to get current user: getting current user: the Telegram client stopped: "+away.Error()+".", texts[0],
				"the tool renders only its failure")
			assert.Equal(t, srv.clientDownText(away), texts[1])
			for _, text := range texts {
				assert.NotContains(t, text, "logout")
				assert.NotContains(t, text, "login")
			}
		})
	}
}

// TestClientDownDefersToACutShortBatch pins what a batch the client stopped
// under reads over HTTP: MarkAsRead reports the chat it marked, the one the
// stop failed and the one it skipped, and the appended answer asks to retry
// only what the call did not complete — it does not claim the call
// completed.
func TestClientDownDefersToACutShortBatch(t *testing.T) {
	srv := &Server{opts: Options{Transport: TransportHTTP}}
	stop := errors.New("connection reset")
	tgClient := newFakeClient()
	resolve := func(id int64) telegramfake.InvokeFunc {
		return telegramfake.Typed(func(_ context.Context, _ *tg.UsersGetUsersRequest, out *tg.UserClassVector) error {
			out.Elems = []tg.UserClass{&tg.User{ID: id, AccessHash: id}}
			return nil
		})
	}
	api := tg.NewClient(telegramfake.New(
		resolve(1), resolve(2), resolve(3),
		telegramfake.Typed(func(_ context.Context, _ *tg.MessagesReadHistoryRequest, out *tg.MessagesAffectedMessages) error {
			return nil
		}),
		// The client fails the calls in flight with why it stopped.
		telegramfake.Typed(func(context.Context, *tg.MessagesReadHistoryRequest, *tg.MessagesAffectedMessages) error {
			tgClient.stop(stop)
			return tgclient.Stopped(stop)
		}),
	))

	texts, isError := callThroughClientDown(t, srv, tgClient, tools.NewMessageReadHandler(tgclient.NewResolver(t.Context(), api)), "MarkAsRead",
		map[string]any{"chat_ids": []int64{1, 2, 3}})
	require.False(t, isError, "a batch that returned what it managed stays a success")
	require.Len(t, texts, 2)
	assert.Contains(t, texts[0], `"success_ids":[1]`)
	assert.Contains(t, texts[0], `"skipped_ids":[3]`)
	assert.Equal(t, "The Telegram connection stopped (connection reset); the server reconnects it. Retry what this call did not complete.", texts[1])
}
