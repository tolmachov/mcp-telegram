package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callTool registers tools on a fresh server, connects an in-memory client,
// and calls one tool. Going through the real SDK is the point: the bug being
// guarded against lives in how the SDK serializes handler results.
func callTool(t *testing.T, register func(*mcp.Server), name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	cs := connectToolClient(t, register)
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	return res
}

func connectToolClient(t *testing.T, register func(*mcp.Server)) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	register(srv)

	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "decline"}, nil
		},
	})
	cs, err := client.Connect(ctx, clientT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

// structured decodes a result's StructuredContent into a generic map.
func structured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	require.NotNil(t, res.StructuredContent, "expected structured output")
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

type wrapperOut struct {
	Status string `json:"status"`
}

// TestAddToolNeverSerializesZeroOutput guards the root cause of the empty
// {"status":""} responses: without the wrapper the SDK fills
// StructuredContent from the zero value whenever a handler returns no typed
// output, masking errors and cancellations alike.
func TestAddToolNeverSerializesZeroOutput(t *testing.T) {
	register := func(h mcp.ToolHandlerFor[struct{}, *wrapperOut]) func(*mcp.Server) {
		return func(s *mcp.Server) { AddTool(s, &mcp.Tool{Name: "T"}, h) }
	}

	t.Run("error result carries text and no structured content", func(t *testing.T) {
		res := callTool(t, register(func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, *wrapperOut, error) {
			return errResult("Failed to delete messages: rpc error code 403: MESSAGE_DELETE_FORBIDDEN"), nil, nil
		}), "T", nil)
		assert.True(t, res.IsError)
		assert.Nil(t, res.StructuredContent)
		assert.Contains(t, toolResultText(res), "MESSAGE_DELETE_FORBIDDEN")
	})

	t.Run("non-error result without output is an error", func(t *testing.T) {
		res := callTool(t, register(func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, *wrapperOut, error) {
			return textResult("Cancelled by user."), nil, nil
		}), "T", nil)
		assert.True(t, res.IsError)
		assert.Nil(t, res.StructuredContent)
		assert.Contains(t, toolResultText(res), "returned no result")
	})

	t.Run("typed output passes through", func(t *testing.T) {
		res := callTool(t, register(func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, *wrapperOut, error) {
			return nil, &wrapperOut{Status: "ok"}, nil
		}), "T", nil)
		assert.False(t, res.IsError)
		assert.Equal(t, "ok", structured(t, res)["status"])
	})
}

// gateInvoker fails loudly on every Telegram call. Missing explicit
// confirmation must be rejected before even a resolving/read RPC is issued.
type gateInvoker struct {
	writes int
}

func (f *gateInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	f.writes++
	return fmt.Errorf("gateInvoker: unexpected request %T", input)
}

// TestConfirmGatedToolsFailClosed reproduces the reported bug end to end:
// missing confirm=true must remain an error even when the client advertises
// elicitation, and nothing may reach Telegram.
func TestConfirmGatedToolsFailClosed(t *testing.T) {
	cases := []struct {
		name     string
		register func(*mcp.Server, *tg.Client)
		args     map[string]any
	}{
		{
			name:     "DeleteMessages",
			register: func(s *mcp.Server, c *tg.Client) { NewMessageDeleteHandler(c).Register(s) },
			args:     map[string]any{"chat_id": -testBasicChatID, "message_ids": []string{"559966"}},
		},
		{
			name:     "ForwardMessage",
			register: func(s *mcp.Server, c *tg.Client) { NewMessageForwardHandler(c).Register(s) },
			args:     map[string]any{"from_chat_id": -11, "message_id": "5", "to_chat_id": -12},
		},
		{
			name:     "DeleteFolder",
			register: func(s *mcp.Server, c *tg.Client) { NewDeleteFolderHandler(c).Register(s) },
			args:     map[string]any{"folder_id": 2},
		},
		{
			name:     "LeaveChat",
			register: func(s *mcp.Server, c *tg.Client) { NewLeaveChatHandler(c).Register(s) },
			args:     map[string]any{"chat": "@channel"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := &gateInvoker{}
			client := tg.NewClient(inv)
			res := callTool(t, func(s *mcp.Server) { tc.register(s, client) }, tc.name, tc.args)
			assert.True(t, res.IsError)
			assert.Nil(t, res.StructuredContent)
			assert.Contains(t, toolResultText(res), "confirm")
			assert.Zero(t, inv.writes, "an unconfirmed call must not reach Telegram")
		})
	}
}
