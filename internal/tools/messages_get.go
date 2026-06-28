package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
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
// OffsetID is an opaque message handle (string) returned by a prior call's
// NextOffsetID or pagination hint. It must be a regular-message handle
// ("42"); scheduled handles ("s:...") are rejected because scheduled
// messages don't paginate.
type GetMessagesInput struct {
	ChatID           int64  `json:"chat_id" jsonschema:"The chat ID to get messages from"`
	Limit            int    `json:"limit,omitempty" jsonschema:"Maximum number of regular messages to return (default 50\\, max 100). Does not affect scheduled_messages which are always returned in full."`
	OffsetID         string `json:"offset_id,omitempty" jsonschema:"Opaque message handle to paginate from (copy next_offset_id from a previous response). Only regular-message handles are accepted."`
	FromDate         string `json:"from_date,omitempty" jsonschema:"RFC3339 lower bound (inclusive). Only messages on or after this date are returned. Applied as a post-filter\\, so with from_date the page may contain fewer than limit messages and has_more is forced to false once the window is exhausted."`
	ToDate           string `json:"to_date,omitempty" jsonschema:"RFC3339 exclusive upper bound (strictly-less-than). Only messages strictly before this timestamp are returned. Wired to Telegram's native offset_date parameter. To include a full day\\, pass midnight of the following day\\, e.g. 2026-04-11T00:00:00Z to include all of 2026-04-10."`
	UnreadOnly       bool   `json:"unread_only,omitempty" jsonschema:"Only return unread messages"`
	IncludeScheduled bool   `json:"include_scheduled,omitempty" jsonschema:"Also fetch pending scheduled messages into the separate scheduled_messages field. Default false. Scheduled messages are returned as a full dump (no pagination)."`
}

// messageDTO is the tool-boundary representation of a message. Differs
// from internal/messages.Message in that IDs are opaque string handles
// ("42" for regular, "s:42" for scheduled), so the LLM always works with
// one consistent ID format across tools.
type messageDTO struct {
	ID         string                  `json:"id"`
	ReplyToID  string                  `json:"reply_to_id,omitempty"`
	Date       time.Time               `json:"date"`
	SenderID   int64                   `json:"sender_id,omitempty"`
	SenderName string                  `json:"sender_name,omitempty"`
	Text       string                  `json:"text"`
	Media      *messages.MediaInfo     `json:"media,omitempty"`
	Entities   []string                `json:"entities,omitempty"`
	Reactions  []messages.ReactionInfo `json:"reactions,omitempty"`
}

// getMessagesOutput is the response shape for GetMessages. The separate
// ScheduledMessages field keeps regular-message pagination semantics intact
// while still letting a single tool call surface the full chat picture.
type getMessagesOutput struct {
	ChatID              int64        `json:"chat_id"`
	Messages            []messageDTO `json:"messages"`
	ScheduledMessages   []messageDTO `json:"scheduled_messages,omitempty"`
	Count               int          `json:"count"`
	HasMore             bool         `json:"has_more"`
	NextOffsetID        string       `json:"next_offset_id,omitempty"`
	PaginationHint      string       `json:"pagination_hint,omitempty"`
	ScheduledFetchError string       `json:"scheduled_fetch_error,omitempty"` // non-empty when include_scheduled fetch failed
}

// Register adds the tool to the MCP server.
func (h *MessagesGetHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "GetMessages",
		Description: "Get messages from a specific chat. Returns up to `limit` regular messages (default 50, max 100). Supports pagination via `offset_id` (copy `next_offset_id` from a previous response) and date filtering via `from_date` / `to_date` (RFC3339; `to_date` is exclusive — pass midnight of the next day to include a full day, e.g. 2026-04-11T00:00:00Z to include all of 2026-04-10). Note: `limit` is applied before the `from_date` filter, so with `from_date` set you may receive fewer than `limit` results on the final page. Set `include_scheduled=true` to additionally fetch pending scheduled messages in a separate `scheduled_messages` field — these have opaque handles of the form \"s:<id>\" and are not paginated or affected by date filters. For bulk export, use BackupMessages instead.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *MessagesGetHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in GetMessagesInput) (*mcp.CallToolResult, *getMessagesOutput, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}

	opts := messages.DefaultFetchOptions()
	if in.Limit > 0 {
		opts.Limit = clampLimit(in.Limit, opts.Limit, 100)
	}
	// Parse opaque offset_id handle. Only regular-message handles are
	// meaningful for pagination — scheduled doesn't page.
	if in.OffsetID != "" {
		ref, err := ParseMessageRef(in.OffsetID)
		if err != nil {
			return errInvalidMessageID(in.OffsetID, err), nil, nil
		}
		if ref.Scheduled {
			return errResult("offset_id cannot reference a scheduled message; scheduled history is returned as a single unpaginated dump via include_scheduled=true"), nil, nil
		}
		opts.OffsetID = ref.ID
	}
	opts.UnreadOnly = in.UnreadOnly

	// Date filters. to_date maps to Telegram's native offset_date; from_date
	// has no native equivalent and is applied as a post-filter inside the
	// provider (drops messages older than MinDate and clamps HasMore).
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
		Messages: make([]messageDTO, 0, len(result.Messages)),
		Count:    result.Count,
		HasMore:  result.HasMore,
	}

	for _, m := range result.Messages {
		out.Messages = append(out.Messages, toMessageDTO(m, false))
	}

	if result.HasMore && result.NextID > 0 {
		out.NextOffsetID = FormatRegularRef(result.NextID)
		out.PaginationHint = fmt.Sprintf("More messages available. Call GetMessages again with offset_id=%q to fetch the next page.", out.NextOffsetID)
	}

	// Optionally fetch scheduled messages. A failure in the scheduled sub-fetch
	// should not fail the whole tool — log a warning and continue with an
	// empty list so the caller still gets the regular history.
	if in.IncludeScheduled {
		// Always initialize as a non-nil empty slice so JSON emits "[]" rather
		// than omitting the field — clients/LLMs then see an unambiguous
		// "no pending scheduled messages" signal.
		out.ScheduledMessages = make([]messageDTO, 0)
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
				out.ScheduledMessages = append(out.ScheduledMessages, toMessageDTO(m, true))
			}
		}
	}

	return nil, out, nil
}

// toMessageDTO converts an internal messages.Message to its tool-boundary
// form, formatting the ID as an opaque handle. When scheduled is true, the
// message is formatted with the "s:" prefix. ReplyToID is always formatted
// as a regular handle (invariant: scheduled messages cannot be reply
// targets of anything the user would hold a handle to).
func toMessageDTO(m messages.Message, scheduled bool) messageDTO {
	var id string
	if scheduled {
		id = FormatScheduledRef(m.ID)
	} else {
		id = FormatRegularRef(m.ID)
	}
	dto := messageDTO{
		ID:         id,
		Date:       m.Date,
		SenderID:   m.SenderID,
		SenderName: m.SenderName,
		Text:       m.Text,
		Media:      m.Media,
		Entities:   m.Entities,
		Reactions:  m.Reactions,
	}
	if m.ReplyToID > 0 {
		dto.ReplyToID = FormatRegularRef(m.ReplyToID)
	}
	return dto
}
