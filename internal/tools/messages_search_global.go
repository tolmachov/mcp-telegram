package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/presentation"
)

// MessagesSearchGlobalHandler handles the SearchMessagesGlobal tool.
type MessagesSearchGlobalHandler struct {
	provider *messages.Provider
}

// NewMessagesSearchGlobalHandler creates a new MessagesSearchGlobalHandler.
func NewMessagesSearchGlobalHandler(provider *messages.Provider) *MessagesSearchGlobalHandler {
	return &MessagesSearchGlobalHandler{provider: provider}
}

// SearchMessagesGlobalInput is the input for the SearchMessagesGlobal tool.
//
// Pagination uses an opaque Cursor string rather than a plain offset_id
// because messages.searchGlobal pagination is a tuple of (offset_rate,
// offset_peer, offset_id) that must be kept together — the cursor carries
// all three so callers never have to know the internal shape.
type SearchMessagesGlobalInput struct {
	Query    string `json:"query" jsonschema:"Substring to search for across all chats the user participates in."`
	Limit    int    `json:"limit,omitempty" jsonschema:"Maximum number of results to return (default 50\\, max 100)."`
	Cursor   string `json:"cursor,omitempty" jsonschema:"Opaque pagination cursor. Copy next_cursor from a previous response verbatim. Do not construct or parse manually."`
	FromDate string `json:"from_date,omitempty" jsonschema:"RFC3339 lower bound (inclusive)."`
	ToDate   string `json:"to_date,omitempty" jsonschema:"RFC3339 exclusive upper bound (strictly less-than). To include a full day\\, pass midnight of the following day\\, e.g. 2026-04-11T00:00:00Z to include all of 2026-04-10."`
}

// globalMessageDTO is the tool-boundary representation of a message returned
// from cross-chat search. It carries chat attribution inline since results
// span many chats. The embedded presentation.Message keeps the regular-message
// shape (opaque id handle, sender, text, media, entities) identical to
// GetMessages output, so downstream tools can consume the shape
// interchangeably.
type globalMessageDTO struct {
	presentation.Message
	ChatID    int64  `json:"chat_id"`
	ChatTitle string `json:"chat_title,omitempty"`
}

// searchMessagesGlobalOutput is the response shape for SearchMessagesGlobal.
type searchMessagesGlobalOutput struct {
	Query      string             `json:"query"`
	Messages   []globalMessageDTO `json:"messages"`
	Count      int                `json:"count"`
	HasMore    bool               `json:"has_more"`
	NextCursor string             `json:"next_cursor,omitempty"`
	// SkippedCount is non-zero when the raw page contained items that
	// couldn't be rendered as regular messages (service messages, empty
	// slots, or unknown peer classes). Surfaced so a page of, say, 50
	// service messages doesn't look like "zero results" to the LLM.
	SkippedCount   int    `json:"skipped_count,omitempty"`
	PaginationHint string `json:"pagination_hint,omitempty"`
}

// Register adds the SearchMessagesGlobal tool to the MCP server.
func (h *MessagesSearchGlobalHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name: "SearchMessagesGlobal",
		Description: "Search messages by substring across ALL chats the user participates in, via Telegram's messages.searchGlobal. Returns up to `limit` matches (default 50, max 100) from any chat, each tagged with its own chat_id and chat_title. " +
			"Supports date range via `from_date` / `to_date` (RFC3339; `to_date` is exclusive — pass midnight of the next day to include a full day) and pagination via the opaque `cursor` string (copy `next_cursor` from a previous response). " +
			"For searching inside one specific chat use SearchMessages instead — it exposes more filters (sender, media type, thread). " +
			"Telegram's usual flood limits apply; there is no per-day quota on this method.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *MessagesSearchGlobalHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in SearchMessagesGlobalInput) (*mcp.CallToolResult, *searchMessagesGlobalOutput, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return ErrResult("query is required and must be a non-empty string."), nil, nil
	}

	opts := messages.GlobalSearchOptions{
		Query: query,
		Limit: clampLimit(in.Limit, 50, 100),
	}

	if in.Cursor != "" {
		cursor, err := ParseGlobalSearchCursor(in.Cursor)
		if err != nil {
			return ErrResult(fmt.Sprintf(
				"invalid cursor: %v. The cursor must be copied verbatim from a prior response's next_cursor — do not construct or modify it. To restart pagination, omit the cursor entirely.",
				err,
			)), nil, nil
		}
		opts.Cursor = cursor
	}

	var dateErr *mcp.CallToolResult
	opts.MinDate, opts.MaxDate, dateErr = parseDateWindow(in.FromDate, in.ToDate)
	if dateErr != nil {
		return dateErr, nil, nil
	}

	result, err := h.provider.SearchGlobal(ctx, opts)
	if err != nil {
		return nil, nil, failed("search globally", err)
	}

	out := &searchMessagesGlobalOutput{
		Query:        query,
		Messages:     make([]globalMessageDTO, 0, len(result.Messages)),
		Count:        result.Count,
		HasMore:      result.HasMore,
		SkippedCount: result.SkippedCount,
	}

	for _, gm := range result.Messages {
		out.Messages = append(out.Messages, globalMessageDTO{
			Message:   presentation.FromMessage(gm.Message, false),
			ChatID:    gm.ChatID,
			ChatTitle: gm.ChatTitle,
		})
	}

	if result.HasMore && result.NextCursor != nil {
		out.NextCursor = FormatGlobalSearchCursor(*result.NextCursor)
		out.PaginationHint = "More matches available. Call SearchMessagesGlobal again with cursor=next_cursor to fetch the next page."
	}

	return nil, out, nil
}
