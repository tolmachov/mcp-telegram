package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/presentation"
)

// MessagesSearchHandler handles the SearchMessages tool.
type MessagesSearchHandler struct {
	provider *messages.Provider
}

// NewMessagesSearchHandler creates a new MessagesSearchHandler.
func NewMessagesSearchHandler(provider *messages.Provider) *MessagesSearchHandler {
	return &MessagesSearchHandler{provider: provider}
}

// SearchMessagesInput is the input for the SearchMessages tool.
//
// ChatID and Query are required on the first page. Cursor is the opaque
// continuation token returned by a prior response. TopMsgID is the handle of
// a forum topic or thread root. FromSenderID is a plain numeric user/chat
// ID (not an opaque message handle — it's a peer, looked up via the same
// path as ChatID). MediaType is a whitelisted enum; leave empty for plain
// text search across all message types.
type SearchMessagesInput struct {
	ChatID          int64  `json:"chat_id,omitempty" jsonschema:"Required on the first page; omit when passing cursor. The chat ID to search in"`
	Query           string `json:"query,omitempty" jsonschema:"Required on the first page; omit when passing cursor. Substring to search for. Telegram's server-side search does token/prefix matching\\, not arbitrary regex."`
	Limit           int    `json:"limit,omitempty" jsonschema:"Maximum number of results to return (default 50\\, max 100)."`
	Cursor          string `json:"cursor,omitempty" jsonschema:"Opaque continuation cursor. On continuation pass only this field; all original filters are embedded in it."`
	BeforeMessageID string `json:"before_message_id,omitempty" jsonschema:"For the first page only: search strictly before this regular-message handle."`
	FromDate        string `json:"from_date,omitempty" jsonschema:"RFC3339 lower bound (inclusive). Wired to Telegram's native min_date (inclusive)."`
	ToDate          string `json:"to_date,omitempty" jsonschema:"RFC3339 exclusive upper bound. Wired to Telegram's native max_date (strictly less-than). To include a full day\\, pass midnight of the following day\\, e.g. 2026-04-11T00:00:00Z to include all of 2026-04-10."`
	FromSenderID    int64  `json:"from_sender_id,omitempty" jsonschema:"Numeric peer ID of a sender to filter by (e.g. from ResolveUsername or GetChatInfo). Only returns messages authored by this user/channel."`
	MediaType       string `json:"media_type,omitempty" jsonschema:"Optional message-type filter. Omit for plain text search."`
	TopMsgID        string `json:"top_msg_id,omitempty" jsonschema:"Opaque regular-message handle of a forum topic or reply thread root. When set\\, results are restricted to that thread."`
}

// searchMessagesOutput mirrors getMessagesOutput but is declared separately
// so schema generation doesn't alias the two tools. The per-message DTO
// shape is identical (both use presentation.Message) so downstream tools
// (EditMessage, DeleteMessages, GetMessageContext) can consume either.
// The envelope differs: this one adds Query, NextCursor, and PaginationHint
// for search-specific pagination.
type searchMessagesOutput struct {
	ChatID         int64                  `json:"chat_id"`
	Query          string                 `json:"query"`
	Messages       []presentation.Message `json:"messages"`
	Count          int                    `json:"count"`
	HasMore        bool                   `json:"has_more"`
	NextCursor     string                 `json:"next_cursor,omitempty"`
	PaginationHint string                 `json:"pagination_hint,omitempty"`
}

var searchMessagesInputSchema = inputSchemaWithEnums[SearchMessagesInput](map[string][]string{
	"media_type": mediaTypes,
})

// Register adds the SearchMessages tool to the MCP server.
func (h *MessagesSearchHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name: "SearchMessages",
		Description: "Search messages by substring within a specific chat via Telegram's server-side messages.search. Returns up to `limit` messages (default 50, max 100) sorted newest-first. " +
			"Continue with `cursor` alone; it embeds the original query and filters. Use `before_message_id` only for the initial anchor. Date range uses inclusive `from_date` and exclusive `to_date`. " +
			"For cross-chat search use SearchMessagesGlobal. For chat discovery by title use SearchChats.",
		InputSchema: searchMessagesInputSchema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *MessagesSearchHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in SearchMessagesInput) (*mcp.CallToolResult, *searchMessagesOutput, error) {
	page, invalid := prepareSearchPage(in)
	if invalid != nil {
		return invalid, nil, nil
	}

	result, err := h.provider.Search(ctx, page.chatID, page.options)
	if err != nil {
		return nil, nil, failed(fmt.Sprintf("search messages in chat %d", page.chatID), err)
	}

	out := &searchMessagesOutput{
		ChatID:   page.chatID,
		Query:    page.options.Query,
		Messages: presentation.FromMessages(result.Messages, false),
		Count:    result.Count,
		HasMore:  result.HasMore,
	}
	out.NextCursor, out.PaginationHint = page.next(result)

	return nil, out, nil
}
