package server

import (
	"context"
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
			server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
			server.AddReceivingMiddleware(srv.clientDownMiddleware(tgClient))
			tools.NewMeGetHandler(api).Register(server)

			serverT, clientT := mcp.NewInMemoryTransports()
			ss, err := server.Connect(t.Context(), serverT, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = ss.Close() })
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(t.Context(), clientT, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = cs.Close() })

			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "GetMe", Arguments: map[string]any{}})
			require.NoError(t, err)
			require.True(t, res.IsError)
			require.Len(t, res.Content, 2)
			assert.Equal(t, "Failed to get current user: getting current user: telegram session is not authorized: rpc error code 401: SESSION_REVOKED.",
				res.Content[0].(*mcp.TextContent).Text, "the tool renders only its failure")
			assert.Equal(t, srv.clientDownText(dead, false), res.Content[1].(*mcp.TextContent).Text, "the server explains the dead session")
		})
	}
}
