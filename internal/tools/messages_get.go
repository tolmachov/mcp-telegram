package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/presentation"
)

// MessagesGetHandler handles the GetMessages tool.
type MessagesGetHandler struct {
	provider *messages.Provider
}

// NewMessagesGetHandler creates a new MessagesGetHandler.
func NewMessagesGetHandler(provider *messages.Provider) *MessagesGetHandler {
	return &MessagesGetHandler{provider: provider}
}

// GetMessagesInput is the input for the GetMessages tool.
//
// Cursor is the opaque continuation returned by a prior call. For an explicit
// first-page anchor, BeforeMessageID must be a regular-message handle ("42");
// scheduled handles ("s:...") are rejected because they do not paginate.
type GetMessagesInput struct {
	ChatID           int64  `json:"chat_id,omitempty" jsonschema:"Required on the first page; omit when passing cursor. The chat ID to get messages from"`
	Limit            int    `json:"limit,omitempty" jsonschema:"Maximum number of regular messages to return (default 50\\, max 100). Does not affect scheduled_messages which are always returned in full."`
	Cursor           string `json:"cursor,omitempty" jsonschema:"Opaque continuation cursor. On continuation pass only this field; all original filters are embedded in it."`
	BeforeMessageID  string `json:"before_message_id,omitempty" jsonschema:"For the first page only: start strictly before this regular-message handle. Use cursor for subsequent pages."`
	FromDate         string `json:"from_date,omitempty" jsonschema:"RFC3339 lower bound (inclusive). Only messages on or after this date are returned. Applied as a post-filter\\, so with from_date the page may contain fewer than limit messages and has_more is forced to false once the window is exhausted."`
	ToDate           string `json:"to_date,omitempty" jsonschema:"RFC3339 exclusive upper bound (strictly-less-than). Only messages strictly before this timestamp are returned. Wired to Telegram's native offset_date parameter. To include a full day\\, pass midnight of the following day\\, e.g. 2026-04-11T00:00:00Z to include all of 2026-04-10."`
	UnreadOnly       bool   `json:"unread_only,omitempty" jsonschema:"Only return unread messages"`
	IncludeScheduled bool   `json:"include_scheduled,omitempty" jsonschema:"Also fetch pending scheduled messages into the separate scheduled_messages field. Default false. Scheduled messages are returned as a full dump (no pagination)."`
}

// getMessagesOutput is the response shape for GetMessages. The separate
// ScheduledMessages field keeps regular-message pagination semantics intact
// while still letting a single tool call surface the full chat picture.
type getMessagesOutput struct {
	ChatID            int64                  `json:"chat_id"`
	Messages          []presentation.Message `json:"messages"`
	ScheduledMessages []presentation.Message `json:"scheduled_messages,omitzero"`
	Count             int                    `json:"count"`
	HasMore           bool                   `json:"has_more"`
	NextCursor        string                 `json:"next_cursor,omitempty"`
	PaginationHint    string                 `json:"pagination_hint,omitempty"`
	// The warning is set when include_scheduled could not fetch the
	// scheduled messages.
	partialOutcome
}

// Register adds the tool to the MCP server.
func (h *MessagesGetHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name:        "GetMessages",
		Description: "Get messages from a specific chat. Returns up to `limit` regular messages (default 50, max 100). Continue with `cursor` alone; it embeds the original chat and filters. Use `before_message_id` only to choose the first-page anchor. Date filtering uses inclusive `from_date` and exclusive `to_date`. Set `include_scheduled=true` to additionally fetch pending scheduled messages in a separate field.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *MessagesGetHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in GetMessagesInput) (*mcp.CallToolResult, *getMessagesOutput, error) {
	page, invalid := prepareHistoryPage(in)
	if invalid != nil {
		return invalid, nil, nil
	}

	result, err := h.provider.Fetch(ctx, page.chatID, page.options)
	if err != nil {
		return nil, nil, failed(fmt.Sprintf("get messages from chat %d", page.chatID), err)
	}

	out := &getMessagesOutput{
		ChatID:   page.chatID,
		Messages: presentation.FromMessages(result.Messages, false),
		Count:    result.Count,
		HasMore:  result.HasMore,
	}
	out.NextCursor, out.PaginationHint = page.next(result)

	// Optionally fetch scheduled messages. A failure in the scheduled sub-fetch
	// should not fail the whole tool: the caller still gets the regular
	// history, with a warning that the scheduled list is missing.
	if page.includeScheduled {
		// A non-nil empty slice is no zero value, so JSON emits "[]" rather
		// than omitting the field (omitzero) — clients/LLMs then see an
		// unambiguous "no pending scheduled messages" signal.
		out.ScheduledMessages = make([]presentation.Message, 0)
		scheduled, err := h.provider.FetchScheduled(ctx, page.chatID)
		if err != nil {
			// An empty slice alone is indistinguishable from "no pending
			// scheduled messages", so the warning says it is not that.
			out.warnCause("GetMessages", "The scheduled messages could not be fetched, so scheduled_messages says nothing about them:", err, "")
		} else {
			out.ScheduledMessages = presentation.FromMessages(scheduled.Messages, true)
		}
	}

	return nil, out, nil
}
