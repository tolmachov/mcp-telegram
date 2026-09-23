package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/presentation"
)

// RepliesGetHandler handles the GetReplies tool.
type RepliesGetHandler struct {
	provider *messages.Provider
}

// NewGetRepliesHandler creates a new RepliesGetHandler.
func NewGetRepliesHandler(provider *messages.Provider) *RepliesGetHandler {
	return &RepliesGetHandler{provider: provider}
}

// GetRepliesInput is the input for the GetReplies tool.
//
// MessageID is an opaque regular-message handle identifying the thread root:
// the channel post (to read its comments) or the forum topic (to read the
// topic's messages — pass the topic id from GetForumTopics). Scheduled handles
// are rejected because they have no thread.
type GetRepliesInput struct {
	ChatID          int64  `json:"chat_id,omitempty" jsonschema:"Required on the first page; omit when passing cursor. The channel ID (for post comments) or forum supergroup ID (for topic messages)"`
	MessageID       string `json:"message_id,omitempty" jsonschema:"Required on the first page; omit when passing cursor. Opaque regular-message handle of the thread root: a channel post ID (to read its comments) or a forum topic ID from GetForumTopics (to read the topic's messages)."`
	Limit           int    `json:"limit,omitempty" jsonschema:"Maximum number of messages to return (default 50\\, max 100)."`
	Cursor          string `json:"cursor,omitempty" jsonschema:"Opaque continuation cursor. On continuation pass only this field; the chat, thread root, and filters are embedded in it."`
	BeforeMessageID string `json:"before_message_id,omitempty" jsonschema:"For the first page only: start strictly before this regular-message handle."`
	FromDate        string `json:"from_date,omitempty" jsonschema:"RFC3339 lower bound (inclusive). Applied as a post-filter\\, so the page may contain fewer than limit messages and has_more is forced to false once the window is exhausted."`
	ToDate          string `json:"to_date,omitempty" jsonschema:"RFC3339 exclusive upper bound (strictly-less-than). Wired to Telegram's native offset_date. To include a full day\\, pass midnight of the following day."`
}

// getRepliesOutput mirrors getMessagesOutput's regular-message envelope (same
// presentation.Message shape so downstream tools can consume either) with the
// thread root echoed back for context.
type getRepliesOutput struct {
	ChatID         int64                  `json:"chat_id"`
	MessageID      string                 `json:"message_id"`
	Messages       []presentation.Message `json:"messages"`
	Count          int                    `json:"count"`
	HasMore        bool                   `json:"has_more"`
	NextCursor     string                 `json:"next_cursor,omitempty"`
	PaginationHint string                 `json:"pagination_hint,omitempty"`
}

// Register adds the GetReplies tool to the MCP server.
func (h *RepliesGetHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name: "GetReplies",
		Description: "Read the discussion thread under a message via Telegram's messages.getReplies. Two uses, same call: " +
			"(1) comments under a channel post — pass the channel `chat_id` and the post's `message_id`; " +
			"(2) messages inside a forum-supergroup topic — pass the supergroup `chat_id` and the topic id (the `id` field from GetForumTopics) as `message_id`. " +
			"Returns up to `limit` messages (default 50, max 100) newest-first. Continue with `cursor` alone; use `before_message_id` only for an initial anchor. Date filtering uses inclusive `from_date` and exclusive `to_date`. " +
			"A message's `replies.count` (from GetMessages/SearchMessages) tells you how many comments a post has before you fetch them here. " +
			"An empty result (count 0) may mean the message has no thread, comments are disabled, or the id is wrong.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *RepliesGetHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in GetRepliesInput) (*mcp.CallToolResult, *getRepliesOutput, error) {
	opts := messages.DefaultFetchOptions()
	state := messagePageCursor{Kind: cursorKindReplies}
	var rootID int
	if in.Cursor != "" {
		if in.ChatID != 0 || in.MessageID != "" || in.Limit != 0 || in.BeforeMessageID != "" || in.FromDate != "" || in.ToDate != "" {
			return errResult("cursor is incompatible with every other field; pass the cursor alone"), nil, nil
		}
		var err error
		state, err = parseMessagePageCursor(in.Cursor, cursorKindReplies)
		if err != nil {
			return errResult(fmt.Sprintf("invalid cursor: %v", err)), nil, nil
		}
		in.ChatID, in.Limit = state.ChatID, state.Limit
		in.FromDate, in.ToDate = state.FromDate, state.ToDate
		rootID = state.RootMessageID
		opts.OffsetID, opts.Limit = state.OffsetID, state.Limit
	} else {
		if in.ChatID == 0 {
			return errChatIDRequired(), nil, nil
		}
		var errRes *mcp.CallToolResult
		rootID, errRes = parseRegularRef("message_id", in.MessageID, "read replies to")
		if errRes != nil {
			return errRes, nil, nil
		}
		opts.Limit = clampLimit(in.Limit, opts.Limit, 100)
		if in.BeforeMessageID != "" {
			opts.OffsetID, errRes = parseRegularRef("before_message_id", in.BeforeMessageID, "page before")
			if errRes != nil {
				return errRes, nil, nil
			}
		}
	}

	var dateErr *mcp.CallToolResult
	opts.MinDate, opts.MaxDate, dateErr = parseDateWindow(in.FromDate, in.ToDate)
	if dateErr != nil {
		return dateErr, nil, nil
	}
	if in.Cursor == "" {
		state = messagePageCursor{Kind: cursorKindReplies, ChatID: in.ChatID, Limit: opts.Limit, FromDate: in.FromDate, ToDate: in.ToDate, RootMessageID: rootID}
	}

	result, err := h.provider.FetchReplies(ctx, in.ChatID, rootID, opts)
	if err != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "GetReplies", map[string]any{
			"action":     "provider_fetch_replies_failed",
			"chat_id":    in.ChatID,
			"message_id": in.MessageID,
			"error":      err.Error(),
		})
		return errResult(fmt.Sprintf("Failed to get replies for message %s in chat %d: %v. Make sure the message has a comment thread (channel post with discussion) or is a forum topic id, and that you have access.", in.MessageID, in.ChatID, err)), nil, nil
	}

	out := &getRepliesOutput{
		ChatID:    in.ChatID,
		MessageID: presentation.FormatRegularRef(rootID),
		Messages:  make([]presentation.Message, 0, len(result.Messages)),
		Count:     result.Count,
		HasMore:   result.HasMore,
	}
	for _, m := range result.Messages {
		out.Messages = append(out.Messages, presentation.FromMessage(m, false))
	}
	if result.HasMore && result.NextID > 0 {
		state.OffsetID = result.NextID
		out.NextCursor = formatMessagePageCursor(state)
		out.PaginationHint = "More replies available. Call GetReplies again with next_cursor copied verbatim into cursor and omit every other field."
	}

	return nil, out, nil
}
