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
	ChatID              int64                  `json:"chat_id"`
	Messages            []presentation.Message `json:"messages"`
	ScheduledMessages   []presentation.Message `json:"scheduled_messages,omitempty"`
	Count               int                    `json:"count"`
	HasMore             bool                   `json:"has_more"`
	NextCursor          string                 `json:"next_cursor,omitempty"`
	PaginationHint      string                 `json:"pagination_hint,omitempty"`
	ScheduledFetchError string                 `json:"scheduled_fetch_error,omitempty"` // non-empty when include_scheduled fetch failed
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
	opts := messages.DefaultFetchOptions()
	state := messagePageCursor{Kind: cursorKindHistory}
	if in.Cursor != "" {
		if in.ChatID != 0 || in.Limit != 0 || in.BeforeMessageID != "" || in.FromDate != "" || in.ToDate != "" || in.UnreadOnly || in.IncludeScheduled {
			return errResult("cursor is incompatible with every other field; pass the cursor alone"), nil, nil
		}
		var err error
		state, err = parseMessagePageCursor(in.Cursor, cursorKindHistory)
		if err != nil {
			return errResult(fmt.Sprintf("invalid cursor: %v", err)), nil, nil
		}
		in.ChatID, in.Limit = state.ChatID, state.Limit
		in.FromDate, in.ToDate = state.FromDate, state.ToDate
		in.UnreadOnly, in.IncludeScheduled = state.UnreadOnly, state.IncludeScheduled
		opts.OffsetID = state.OffsetID
		opts.Limit = state.Limit
	} else {
		if in.ChatID == 0 {
			return errChatIDRequired(), nil, nil
		}
		opts.Limit = clampLimit(in.Limit, opts.Limit, 100)
		if in.BeforeMessageID != "" {
			var errRes *mcp.CallToolResult
			opts.OffsetID, errRes = parseRegularRef("before_message_id", in.BeforeMessageID, "page before")
			if errRes != nil {
				return errRes, nil, nil
			}
		}
		state = messagePageCursor{Kind: cursorKindHistory, ChatID: in.ChatID, Limit: opts.Limit, FromDate: in.FromDate, ToDate: in.ToDate, UnreadOnly: in.UnreadOnly, IncludeScheduled: in.IncludeScheduled}
	}
	opts.UnreadOnly = in.UnreadOnly

	// Date filters. to_date maps to Telegram's native offset_date; from_date
	// has no native equivalent and is applied as a post-filter inside the
	// provider (drops messages older than MinDate and clamps HasMore).
	var dateErr *mcp.CallToolResult
	opts.MinDate, opts.MaxDate, dateErr = parseDateWindow(in.FromDate, in.ToDate)
	if dateErr != nil {
		return dateErr, nil, nil
	}

	result, err := h.provider.Fetch(ctx, in.ChatID, opts)
	if err != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "GetMessages", map[string]any{
			"action":  "provider_fetch_failed",
			"chat_id": in.ChatID,
			"error":   err.Error(),
		})
		return errResult(fmt.Sprintf("Failed to get messages: %v", err)), nil, nil
	}

	out := &getMessagesOutput{
		ChatID:   in.ChatID,
		Messages: make([]presentation.Message, 0, len(result.Messages)),
		Count:    result.Count,
		HasMore:  result.HasMore,
	}

	for _, m := range result.Messages {
		out.Messages = append(out.Messages, presentation.FromMessage(m, false))
	}

	if result.HasMore && result.NextID > 0 {
		state.OffsetID = result.NextID
		out.NextCursor = formatMessagePageCursor(state)
		out.PaginationHint = "More messages available. Call GetMessages again with next_cursor copied verbatim into cursor and omit every other field."
	}

	// Optionally fetch scheduled messages. A failure in the scheduled sub-fetch
	// should not fail the whole tool — log a warning and continue with an
	// empty list so the caller still gets the regular history.
	if in.IncludeScheduled {
		// Always initialize as a non-nil empty slice so JSON emits "[]" rather
		// than omitting the field — clients/LLMs then see an unambiguous
		// "no pending scheduled messages" signal.
		out.ScheduledMessages = make([]presentation.Message, 0)
		scheduled, err := h.provider.FetchScheduled(ctx, in.ChatID)
		if err != nil {
			// Log and surface the error so the LLM knows the scheduled list may
			// be incomplete — an empty slice alone is indistinguishable from
			// "no pending scheduled messages".
			mcpLog(ctx, req.Session, logLevelWarning, "GetMessages", map[string]any{
				"action":  "include_scheduled_fetch_failed",
				"chat_id": in.ChatID,
				"error":   err.Error(),
			})
			out.ScheduledFetchError = fmt.Sprintf("scheduled messages unavailable: %v", err)
		} else {
			for _, m := range scheduled.Messages {
				out.ScheduledMessages = append(out.ScheduledMessages, presentation.FromMessage(m, true))
			}
		}
	}

	return nil, out, nil
}
