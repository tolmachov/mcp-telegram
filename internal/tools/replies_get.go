package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
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
	ChatID    int64  `json:"chat_id" jsonschema:"The channel ID (for post comments) or forum supergroup ID (for topic messages)"`
	MessageID string `json:"message_id" jsonschema:"Opaque regular-message handle of the thread root: a channel post ID (to read its comments) or a forum topic ID from GetForumTopics (to read the topic's messages)."`
	Limit     int    `json:"limit,omitempty" jsonschema:"Maximum number of messages to return (default 50\\, max 100)."`
	OffsetID  string `json:"offset_id,omitempty" jsonschema:"Opaque message handle to paginate from (copy next_offset_id from a previous response). Only regular-message handles are accepted."`
	FromDate  string `json:"from_date,omitempty" jsonschema:"RFC3339 lower bound (inclusive). Applied as a post-filter\\, so the page may contain fewer than limit messages and has_more is forced to false once the window is exhausted."`
	ToDate    string `json:"to_date,omitempty" jsonschema:"RFC3339 exclusive upper bound (strictly-less-than). Wired to Telegram's native offset_date. To include a full day\\, pass midnight of the following day."`
}

// getRepliesOutput mirrors getMessagesOutput's regular-message envelope (same
// messageDTO shape so downstream tools can consume either) with the thread
// root echoed back for context.
type getRepliesOutput struct {
	ChatID         int64        `json:"chat_id"`
	MessageID      string       `json:"message_id"`
	Messages       []messageDTO `json:"messages"`
	Count          int          `json:"count"`
	HasMore        bool         `json:"has_more"`
	NextOffsetID   string       `json:"next_offset_id,omitempty"`
	PaginationHint string       `json:"pagination_hint,omitempty"`
}

// Register adds the GetReplies tool to the MCP server.
func (h *RepliesGetHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "GetReplies",
		Description: "Read the discussion thread under a message via Telegram's messages.getReplies. Two uses, same call: " +
			"(1) comments under a channel post — pass the channel `chat_id` and the post's `message_id`; " +
			"(2) messages inside a forum-supergroup topic — pass the supergroup `chat_id` and the topic id (the `id` field from GetForumTopics) as `message_id`. " +
			"Returns up to `limit` messages (default 50, max 100) newest-first, with pagination via `offset_id` and date filtering via `from_date` / `to_date` (RFC3339; `to_date` exclusive). " +
			"A message's `replies.count` (from GetMessages/SearchMessages) tells you how many comments a post has before you fetch them here. " +
			"An empty result (count 0) may mean the message has no thread, comments are disabled, or the id is wrong.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *RepliesGetHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in GetRepliesInput) (*mcp.CallToolResult, *getRepliesOutput, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}

	rootRef, err := ParseMessageRef(in.MessageID)
	if err != nil {
		return errInvalidMessageID(in.MessageID, err), nil, nil
	}
	if rootRef.Scheduled {
		return errResult("message_id cannot reference a scheduled message; scheduled messages have no reply thread."), nil, nil
	}

	opts := messages.DefaultFetchOptions()
	if in.Limit > 0 {
		opts.Limit = clampLimit(in.Limit, opts.Limit, 100)
	}

	if in.OffsetID != "" {
		ref, err := ParseMessageRef(in.OffsetID)
		if err != nil {
			return errInvalidMessageID(in.OffsetID, err), nil, nil
		}
		if ref.Scheduled {
			return errResult("offset_id cannot reference a scheduled message; reply threads paginate over regular messages only."), nil, nil
		}
		opts.OffsetID = ref.ID
	}

	if in.FromDate != "" {
		t, errRes, ok := parseDateFilter("from_date", in.FromDate)
		if !ok {
			return errRes, nil, nil
		}
		opts.MinDate = t
	}
	if in.ToDate != "" {
		t, errRes, ok := parseDateFilter("to_date", in.ToDate)
		if !ok {
			return errRes, nil, nil
		}
		opts.MaxDate = t
	}
	if !opts.MinDate.IsZero() && !opts.MaxDate.IsZero() && opts.MinDate.After(opts.MaxDate) {
		return errResult(fmt.Sprintf("from_date (%s) is after to_date (%s); the window is empty.", opts.MinDate.Format(time.RFC3339), opts.MaxDate.Format(time.RFC3339))), nil, nil
	}

	result, err := h.provider.FetchReplies(ctx, in.ChatID, rootRef.ID, opts)
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
		MessageID: in.MessageID,
		Messages:  make([]messageDTO, 0, len(result.Messages)),
		Count:     result.Count,
		HasMore:   result.HasMore,
	}
	for _, m := range result.Messages {
		out.Messages = append(out.Messages, toMessageDTO(m, false))
	}
	if result.HasMore && result.NextID > 0 {
		out.NextOffsetID = FormatRegularRef(result.NextID)
		out.PaginationHint = fmt.Sprintf("More replies available. Call GetReplies again with offset_id=%q to fetch the next page.", out.NextOffsetID)
	}

	return nil, out, nil
}
