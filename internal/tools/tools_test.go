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
// wait guidance, the peer hint and the failure's own hint.
func TestFailureText(t *testing.T) {
	flood := &tgerr.Error{Code: 420, Message: "FLOOD_WAIT_265", Type: "FLOOD_WAIT", Argument: 265}
	// The client hands a call the home DC refused the verdict (see
	// tgclient.Running.refusalWatch).
	dead := fmt.Errorf("%w: %w", tgclient.ErrSessionUnauthorized, tgerr.New(401, "AUTH_KEY_UNREGISTERED"))

	t.Run("a failure without a cause still renders", func(t *testing.T) {
		assert.Equal(t, "Failed to send message: the server recorded no cause for this failure (server bug).",
			failureText("SendMessage", failed("send message", nil)))
		assert.Equal(t, "Failed to send message: the server recorded no cause for this failure (server bug). Try again.",
			failureText("SendMessage", failedHint("send message", withHint(nil, ""), "Try again.")))
	})

	t.Run("bare flood wait", func(t *testing.T) {
		txt := failureText("JoinChat", failed(`join "@x"`, flood))
		assert.True(t, strings.HasPrefix(txt, `Failed to join "@x": Telegram rate-limited this JoinChat call: wait 4m25s (265 seconds)`), txt)
	})

	// The client's flood-wait middleware wraps the original error when the
	// wait exceeds its max, so detection must unwrap.
	t.Run("wrapped by the flood-wait middleware (too long)", func(t *testing.T) {
		wrapped := fmt.Errorf("telegram asked for a wait of 4m25s, which with the 0s already waited passes the 1m0s maximum: %w", flood)
		assert.Contains(t, failureText("JoinChat", failed("join", wrapped)), "265 seconds")
	})

	// Every wait the flood-wait middleware takes is rendered as one, in
	// words that fit its kind and never as a wait of nothing.
	t.Run("every kind of wait", func(t *testing.T) {
		for rpcErr, want := range map[*tgerr.Error]string{
			tgerr.New(420, "FLOOD_WAIT_0"):               "Failed to join: Telegram rate-limited this JoinChat call: wait 1s (1 seconds) before retrying. This is an account-level flood limit",
			tgerr.New(420, "FLOOD_PREMIUM_WAIT_5"):       "Failed to join: Telegram rate-limited this JoinChat call: wait 5s (5 seconds)",
			tgerr.New(420, "SLOWMODE_WAIT_10"):           "Failed to join: This chat is in slow mode: wait 10s (10 seconds) before sending to it again.",
			tgerr.New(500, "WORKER_BUSY_TOO_LONG_RETRY"): "Failed to join: Telegram told this JoinChat call to wait 1s (1 seconds) before retrying (WORKER_BUSY_TOO_LONG_RETRY): do not retry sooner.",
			tgerr.New(420, "TAKEOUT_INIT_DELAY_3600"):    "Failed to join: Telegram told this JoinChat call to wait 1h0m0s (3600 seconds) before retrying (TAKEOUT_INIT_DELAY): do not retry sooner.",
		} {
			txt := failureText("JoinChat", failed("join", fmt.Errorf("joining: %w", rpcErr)))
			assert.True(t, strings.HasPrefix(txt, want), txt)
		}
	})

	t.Run("a call whose context ended while it waited", func(t *testing.T) {
		ended := fmt.Errorf("%w while waiting out the 4m25s Telegram asked for (%w)", context.DeadlineExceeded, flood)
		assert.True(t, strings.HasPrefix(failureText("JoinChat", failed("join", ended)), "Failed to join: Telegram rate-limited this JoinChat call: wait 4m25s (265 seconds)"))
	})

	// The server appends the transport's explanation of a dead session (see
	// clientDownText in the server package), so the failure shows only the
	// error.
	t.Run("dead session", func(t *testing.T) {
		assert.Equal(t, "Failed to get current user: telegram session is not authorized: rpc error code 401: AUTH_KEY_UNREGISTERED.",
			failureText("GetMe", failed("get current user", dead)))
	})

	t.Run("a timeout keeps the hint: a smaller request may finish in time", func(t *testing.T) {
		err := failedHint("search messages", fmt.Errorf("searching: %w", context.DeadlineExceeded), "Narrow the date window.")
		assert.Equal(t, "Failed to search messages: searching: context deadline exceeded. Narrow the date window.", failureText("SearchMessages", err))
	})

	t.Run("a chat that cannot be resolved gets the peer hint", func(t *testing.T) {
		err := failed("send message to chat 42", fmt.Errorf("%w 42: no such chat", tgclient.ErrUnresolvablePeer))
		assert.Equal(t, "Failed to send message to chat 42: cannot resolve chat 42: no such chat. "+peerHint, failureText("SendMessage", err))
	})

	t.Run("a resolve that failed for another reason gets no peer hint", func(t *testing.T) {
		err := failed("send message to chat 42", fmt.Errorf("resolving channel 42: %w", tgerr.New(500, "INTERNAL_SERVER_ERROR")))
		assert.Equal(t, "Failed to send message to chat 42: resolving channel 42: rpc error code 500: INTERNAL_SERVER_ERROR.", failureText("SendMessage", err))
	})

	t.Run("own hint follows the error", func(t *testing.T) {
		err := failedHint("get forum topics", errors.New("boom"), "Check the chat is a forum.")
		assert.Equal(t, "Failed to get forum topics: boom. Check the chat is a forum.", failureText("GetForumTopics", err))
	})

	t.Run("inner hint surfaces under the outer op", func(t *testing.T) {
		err := failed("delete messages in chat 5", fmt.Errorf("deleting: %w", withHint(errors.New("forbidden"), "Ask an admin.")))
		assert.Equal(t, "Failed to delete messages in chat 5: deleting: forbidden. Ask an admin.", failureText("DeleteMessages", err))
	})

	t.Run("systemic failures get no hint", func(t *testing.T) {
		for name, cause := range map[string]error{
			"flood wait":     flood,
			"dead session":   dead,
			"stopped client": fmt.Errorf("resolving chat 42: %w: connection reset", tgclient.ErrClientStopped),
			"cancellation":   fmt.Errorf("resolving chat 42: %w", context.Canceled),
		} {
			t.Run(name, func(t *testing.T) {
				txt := failureText("ResolveUsername", failedHint("resolve @x", cause, "Try SearchChats instead."))
				assert.NotContains(t, txt, "Try SearchChats instead.")
				assert.NotContains(t, txt, peerHint)
			})
		}
		assert.Equal(t, "Failed to resolve @x: resolving chat 42: context canceled.",
			failureText("ResolveUsername", failedHint("resolve @x", fmt.Errorf("resolving chat 42: %w", context.Canceled), "Try SearchChats instead.")))
	})

	t.Run("plain error names the tool", func(t *testing.T) {
		assert.Equal(t, "Failed to run GetMe: boom.", failureText("GetMe", errors.New("boom")))
	})
}

// TestPartialOutcome covers the warning of a partial result: its cause is
// rendered as a failure's would be, and AddTool flags it for the request log.
func TestPartialOutcome(t *testing.T) {
	flood := &tgerr.Error{Code: 420, Message: "FLOOD_WAIT_265", Type: "FLOOD_WAIT", Argument: 265}

	var out SearchResultsList
	assert.Nil(t, flagPartial(nil, &out), "an output without a warning is left to the SDK")

	out.warn("The listing is incomplete.")
	out.warnCause("SearchChats", "Global search failed:", fmt.Errorf("searching contacts: %w", flood), "Retry later.")
	assert.True(t, strings.HasPrefix(out.Warning, "The listing is incomplete. Global search failed: Telegram rate-limited this SearchChats call: wait 4m25s (265 seconds)"), out.Warning)
	assert.NotContains(t, out.Warning, "Retry later.", "a hint cannot cure a flood wait")

	res := flagPartial(textResult("found"), &out)
	assert.Equal(t, out.Warning, res.Meta[MetaWarning])
	assert.Equal(t, "found", ResultText(res))

	var plain SearchResultsList
	plain.warnCause("SearchChats", "Global search failed:", errors.New("boom"), "Retry later.")
	assert.Equal(t, "Global search failed: boom. Retry later.", plain.Warning)
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
	assert.Contains(t, ResultText(res), "confirm=true")
}

func TestEveryToolHandlerRegisters(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cache := tgdata.NewChatsCache(t.Context(), nil)
	handlers := []Handler{
		NewMeGetHandler(nil),
		NewChatsGetHandler(cache),
		NewChatsSearchHandler(nil, cache),
		NewChatInfoGetHandler(tgclient.NewResolver(t.Context(), nil)),
		NewMessagesGetHandler(nil),
		NewMessagesSearchHandler(nil),
		NewMessagesSearchGlobalHandler(nil),
		NewMessageContextGetHandler(nil),
		NewGetRepliesHandler(nil),
		NewGetForumTopicsHandler(nil),
		NewUsernameResolveHandler(nil),
		NewMessageLinkResolveHandler(nil),
		NewChatSummarizeHandler(nil, summarize.Unavailable(errors.New("not configured"))),
		NewMediaGetHandler(nil, 1),
		NewGetFoldersHandler(nil),
		NewMessageBackupHandler(tgclient.NewResolver(t.Context(), nil), nil, []string{t.TempDir()}),
		NewMessageSendHandler(tgclient.NewResolver(t.Context(), nil)),
		NewMessageReadHandler(tgclient.NewResolver(t.Context(), nil)),
		NewMessageEditHandler(tgclient.NewResolver(t.Context(), nil)),
		NewMessageDeleteHandler(tgclient.NewResolver(t.Context(), nil)),
		NewMessageForwardHandler(tgclient.NewResolver(t.Context(), nil)),
		NewSetReactionHandler(tgclient.NewResolver(t.Context(), nil)),
		NewJoinChatHandler(tgclient.NewResolver(t.Context(), nil)),
		NewLeaveChatHandler(tgclient.NewResolver(t.Context(), nil)),
		NewChatMuteHandler(tgclient.NewResolver(t.Context(), nil)),
		NewCreateFolderHandler(tgclient.NewResolver(t.Context(), nil)),
		NewDeleteFolderHandler(nil),
		NewAddChatsToFolderHandler(tgclient.NewResolver(t.Context(), nil)),
		NewRemoveChatsFromFolderHandler(tgclient.NewResolver(t.Context(), nil)),
	}
	RegisterTools(server, handlers)
}
