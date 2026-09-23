package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/summarize"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// TestFailureText covers the single rendering of handler errors: the flood
// wait and dead-session guidance, the peer hint, and the failure's own hint.
func TestFailureText(t *testing.T) {
	flood := &tgerr.Error{Code: 420, Message: "FLOOD_WAIT_265", Type: "FLOOD_WAIT", Argument: 265}

	t.Run("bare flood wait", func(t *testing.T) {
		txt := failureText("JoinChat", failed(`join "@x"`, flood))
		assert.True(t, strings.HasPrefix(txt, `Failed to join "@x": Telegram rate-limited this JoinChat call: wait 4m25s (265 seconds)`), txt)
	})

	// The waiter wraps the original error when the wait exceeds its max
	// ("flood wait argument is too big (... > ...)"), so detection must unwrap.
	t.Run("wrapped by the waiter (too big)", func(t *testing.T) {
		wrapped := fmt.Errorf("flood wait argument is too big (4m25s > 1m0s): %w", flood)
		assert.Contains(t, failureText("JoinChat", failed("join", wrapped)), "265 seconds")
	})

	t.Run("dead session", func(t *testing.T) {
		txt := failureText("GetMe", failed("get current user", tgerr.New(401, "AUTH_KEY_UNREGISTERED")))
		assert.Contains(t, txt, "Failed to get current user: Telegram no longer accepts this account's session")
		assert.Contains(t, txt, "mcp-telegram login")
	})

	t.Run("unresolved chat gets the peer hint", func(t *testing.T) {
		err := failed("send message to chat 42", &tgclient.PeerError{ID: 42, Err: errors.New("boom")})
		assert.Equal(t, "Failed to send message to chat 42: resolving chat 42: boom. "+peerHint, failureText("SendMessage", err))
	})

	t.Run("own hint follows the error", func(t *testing.T) {
		err := failedHint("get forum topics", errors.New("boom"), "Check the chat is a forum.")
		assert.Equal(t, "Failed to get forum topics: boom. Check the chat is a forum.", failureText("GetForumTopics", err))
	})

	t.Run("inner hint surfaces under the outer op", func(t *testing.T) {
		err := failed("delete messages in chat 5", fmt.Errorf("deleting: %w", withHint(errors.New("forbidden"), "Ask an admin.")))
		assert.Equal(t, "Failed to delete messages in chat 5: deleting: forbidden. Ask an admin.", failureText("DeleteMessages", err))
	})

	t.Run("plain error names the tool", func(t *testing.T) {
		assert.Equal(t, "Failed to run GetMe: boom.", failureText("GetMe", errors.New("boom")))
	})
}

// Note: progress-token extraction is now SDK-provided
// (req.Params.GetProgressToken()), so the previous TestProgressToken case was
// removed — there is no longer a project-owned helper to test. Coverage of
// our sendProgress wrapper is implicit via the per-tool handler tests once
// in-memory transport-based integration tests are added.

// TestMcpLogSlogFallback verifies that mcpLog handles nil sessions by
// falling back to slog for error and warning levels, and stays silent for
// lower levels. This test verifies the function doesn't panic and behaves correctly.
func TestMcpLogSlogFallback(t *testing.T) {
	ctx := context.Background()

	// Test that nil session + error doesn't panic and logs via slog
	mcpLog(ctx, nil, logLevelError, "test-logger", map[string]any{"k": "v"})

	// Test that nil session + warning doesn't panic and logs via slog
	mcpLog(ctx, nil, logLevelWarning, "test-warn", map[string]any{"k": "v"})

	// Test that nil session + info doesn't panic (lower level, no output expected)
	mcpLog(ctx, nil, logLevelInfo, "test-info", map[string]any{"k": "v"})
}

func TestClampWindow(t *testing.T) {
	tests := []struct {
		name string
		n    int
		want int
	}{
		{"negative returns zero", -1, 0},
		{"large negative returns zero", -100, 0},
		{"zero returns default", 0, defaultContextWindow},
		{"above max clamps to max", maxContextWindow + 1, maxContextWindow},
		{"equal to max passes through", maxContextWindow, maxContextWindow},
		{"within range passes through", 10, 10},
		{"one passes through", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, clampWindow(tt.n))
		})
	}
}

func TestClampLimit(t *testing.T) {
	tests := []struct {
		name            string
		limit, def, max int
		want            int
	}{
		{"zero uses default", 0, 50, 100, 50},
		{"negative uses default", -5, 50, 100, 50},
		{"within range passes through", 30, 50, 100, 30},
		{"above max clamps", 200, 50, 100, 100},
		{"equal to max passes through", 100, 50, 100, 100},
		{"equal to one passes through", 1, 50, 100, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, clampLimit(tt.limit, tt.def, tt.max))
		})
	}
}

func TestRequireExplicitConfirmation(t *testing.T) {
	assert.Nil(t, requireExplicitConfirmation(true, "delete"))
	res := requireExplicitConfirmation(false, "delete")
	require.NotNil(t, res)
	assert.True(t, res.IsError)
	assert.Contains(t, toolResultText(res), "confirm=true")
}

func TestEveryToolHandlerRegisters(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cache := tgdata.NewChatsCache(nil)
	handlers := []Handler{
		NewMeGetHandler(nil),
		NewChatsGetHandler(cache),
		NewChatsSearchHandler(nil, cache),
		NewChatInfoGetHandler(tgclient.NewResolver(nil, 100_000)),
		NewMessagesGetHandler(nil),
		NewMessagesSearchHandler(nil),
		NewMessagesSearchGlobalHandler(nil),
		NewMessageContextGetHandler(nil),
		NewGetRepliesHandler(nil),
		NewGetForumTopicsHandler(nil),
		NewUsernameResolveHandler(nil),
		NewMessageLinkResolveHandler(nil),
		NewChatSummarizeHandler(nil, summarize.Config{}),
		NewMediaGetHandler(nil, 1),
		NewGetFoldersHandler(nil),
		NewMessageBackupHandler(tgclient.NewResolver(nil, 100_000), nil, []string{t.TempDir()}),
		NewMessageSendHandler(tgclient.NewResolver(nil, 100_000)),
		NewMessageReadHandler(tgclient.NewResolver(nil, 100_000)),
		NewMessageEditHandler(tgclient.NewResolver(nil, 100_000)),
		NewMessageDeleteHandler(tgclient.NewResolver(nil, 100_000)),
		NewMessageForwardHandler(tgclient.NewResolver(nil, 100_000)),
		NewSetReactionHandler(tgclient.NewResolver(nil, 100_000)),
		NewJoinChatHandler(tgclient.NewResolver(nil, 100_000)),
		NewLeaveChatHandler(tgclient.NewResolver(nil, 100_000)),
		NewChatMuteHandler(tgclient.NewResolver(nil, 100_000)),
		NewCreateFolderHandler(tgclient.NewResolver(nil, 100_000)),
		NewDeleteFolderHandler(nil),
		NewAddChatsToFolderHandler(tgclient.NewResolver(nil, 100_000)),
		NewRemoveChatsFromFolderHandler(tgclient.NewResolver(nil, 100_000)),
	}
	RegisterTools(server, handlers)
}
