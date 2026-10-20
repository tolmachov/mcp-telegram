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
	page, invalid := prepareRepliesPage(in)
	if invalid != nil {
		return invalid, nil, nil
	}

	result, err := h.provider.FetchReplies(ctx, page.chatID, page.rootID, page.options)
	if err != nil {
		return nil, nil, failedHint(fmt.Sprintf("get replies to message %s in chat %d", presentation.FormatRegularRef(page.rootID), page.chatID), err, "Make sure the message has a comment thread (channel post with discussion) or is a forum topic id, and that you have access.")
	}

	out := &getRepliesOutput{
		ChatID:    page.chatID,
		MessageID: presentation.FormatRegularRef(page.rootID),
		Messages:  presentation.FromMessages(result.Messages, false),
		Count:     result.Count,
		HasMore:   result.HasMore,
	}
	out.NextCursor, out.PaginationHint = page.next(result)

	return nil, out, nil
}
