package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
)

// ForumTopicsGetHandler handles the GetForumTopics tool.
type ForumTopicsGetHandler struct {
	provider *messages.Provider
}

// NewGetForumTopicsHandler creates a new ForumTopicsGetHandler.
func NewGetForumTopicsHandler(provider *messages.Provider) *ForumTopicsGetHandler {
	return &ForumTopicsGetHandler{provider: provider}
}

// GetForumTopicsInput is the input for the GetForumTopics tool.
type GetForumTopicsInput struct {
	ChatID int64  `json:"chat_id" jsonschema:"The forum supergroup ID whose topics to list"`
	Query  string `json:"query,omitempty" jsonschema:"Optional case-insensitive filter on topic title. Leave empty to list all topics."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of topics to return (default 100\\, max 100)."`
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque pagination cursor. Copy next_cursor from a previous response to fetch the next page."`
}

// forumTopicDTO is the tool-boundary representation of a forum topic. Both ID
// and TopMessageID are formatted as opaque regular-message handles, but only
// ID is the thread root fed to GetReplies (message_id) / SearchMessages
// (top_msg_id); TopMessageID just points at the topic's latest message.
type forumTopicDTO struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	TopMessageID string    `json:"top_message_id,omitempty"`
	UnreadCount  int       `json:"unread_count,omitempty"`
	Closed       bool      `json:"closed,omitempty"`
	Pinned       bool      `json:"pinned,omitempty"`
	Hidden       bool      `json:"hidden,omitempty"`
	IconColor    int       `json:"icon_color,omitempty"`
	IconEmojiID  string    `json:"icon_emoji_id,omitempty"`
	Date         time.Time `json:"date"`
}

type getForumTopicsOutput struct {
	ChatID         int64           `json:"chat_id"`
	Topics         []forumTopicDTO `json:"topics"`
	Count          int             `json:"count"`
	HasMore        bool            `json:"has_more"`
	NextCursor     string          `json:"next_cursor,omitempty"`
	PaginationHint string          `json:"pagination_hint,omitempty"`
}

// Register adds the GetForumTopics tool to the MCP server.
func (h *ForumTopicsGetHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "GetForumTopics",
		Description: "List the topics (thematic threads) of a forum supergroup via Telegram's messages.getForumTopics. " +
			"Large thematic communities split their chat into topics; this exposes that structure instead of a flat feed. " +
			"Returns up to `limit` topics (default 100) with pagination via `cursor` and an optional title `query` filter. " +
			"To read the messages inside a topic, pass the topic's `id` to GetReplies as `message_id` (or to SearchMessages as `top_msg_id`). " +
			"Only works on forum-enabled supergroups; non-forum chats return an error, and an empty result means the supergroup has no topics.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *ForumTopicsGetHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in GetForumTopicsInput) (*mcp.CallToolResult, *getForumTopicsOutput, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}

	limit := clampLimit(in.Limit, 100, 100)

	var offsetTopic, offsetID, offsetDate int
	if in.Cursor != "" {
		ot, oi, od, err := ParseForumTopicsCursor(in.Cursor)
		if err != nil {
			return errResult(fmt.Sprintf("invalid cursor: %v. Omit cursor to start from the first page.", err)), nil, nil
		}
		offsetTopic, offsetID, offsetDate = ot, oi, od
	}

	result, err := h.provider.FetchForumTopics(ctx, in.ChatID, in.Query, limit, offsetTopic, offsetID, offsetDate)
	if err != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "GetForumTopics", map[string]any{
			"action":  "provider_fetch_forum_topics_failed",
			"chat_id": in.ChatID,
			"error":   err.Error(),
		})
		return errResult(fmt.Sprintf("Failed to get forum topics for chat %d: %v. This call only works on forum-enabled supergroups; verify the chat is a forum and that you have access.", in.ChatID, err)), nil, nil
	}

	out := &getForumTopicsOutput{
		ChatID:  in.ChatID,
		Topics:  make([]forumTopicDTO, 0, len(result.Topics)),
		Count:   result.Count,
		HasMore: result.NextOffset != nil,
	}
	for _, t := range result.Topics {
		dto := forumTopicDTO{
			Title:       t.Title,
			UnreadCount: t.UnreadCount,
			Closed:      t.Closed,
			Pinned:      t.Pinned,
			Hidden:      t.Hidden,
			IconColor:   t.IconColor,
			IconEmojiID: t.IconEmojiID,
			Date:        t.Date,
		}
		if t.ID > 0 {
			dto.ID = FormatRegularRef(t.ID)
		}
		if t.TopMessageID > 0 {
			dto.TopMessageID = FormatRegularRef(t.TopMessageID)
		}
		out.Topics = append(out.Topics, dto)
	}

	if result.NextOffset != nil {
		out.NextCursor = FormatForumTopicsCursor(result.NextOffset.Topic, result.NextOffset.ID, result.NextOffset.Date)
		out.PaginationHint = fmt.Sprintf("More topics available. Call GetForumTopics again with cursor=%q to fetch the next page.", out.NextCursor)
	}

	return nil, out, nil
}
