package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
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
// ChatID and Query are required. OffsetID is an opaque regular-message
// handle from a prior response's next_offset_id. TopMsgID is the handle of
// a forum topic or thread root. FromSenderID is a plain numeric user/chat
// ID (not an opaque message handle — it's a peer, looked up via the same
// path as ChatID). MediaType is a whitelisted enum; leave empty for plain
// text search across all message types.
type SearchMessagesInput struct {
	ChatID       int64  `json:"chat_id" jsonschema:"The chat ID to search in"`
	Query        string `json:"query" jsonschema:"Substring to search for. Telegram's server-side search does token/prefix matching\\, not arbitrary regex."`
	Limit        int    `json:"limit,omitempty" jsonschema:"Maximum number of results to return (default 50\\, max 100)."`
	OffsetID     string `json:"offset_id,omitempty" jsonschema:"Opaque message handle to paginate from (copy next_offset_id from a previous response). Only regular-message handles are accepted."`
	FromDate     string `json:"from_date,omitempty" jsonschema:"RFC3339 lower bound (inclusive). Wired to Telegram's native min_date (inclusive)."`
	ToDate       string `json:"to_date,omitempty" jsonschema:"RFC3339 exclusive upper bound. Wired to Telegram's native max_date (strictly less-than). To include a full day\\, pass midnight of the following day\\, e.g. 2026-04-11T00:00:00Z to include all of 2026-04-10."`
	FromSenderID int64  `json:"from_sender_id,omitempty" jsonschema:"Numeric peer ID of a sender to filter by (e.g. from ResolveUsername or GetChatInfo). Only returns messages authored by this user/channel."`
	MediaType    string `json:"media_type,omitempty" jsonschema:"Optional message-type filter. One of: photos\\, videos\\, documents\\, links\\, voice\\, music\\, gif\\, round_video\\, round_voice. Leave empty for plain text search."`
	TopMsgID     string `json:"top_msg_id,omitempty" jsonschema:"Opaque regular-message handle of a forum topic or reply thread root. When set\\, results are restricted to that thread."`
}

// mediaFilterMap translates the whitelisted media_type enum into its
// corresponding tg.MessagesFilterClass constructor. Keys MUST stay in
// sync with the jsonschema description on SearchMessagesInput.MediaType.
var mediaFilterMap = map[string]func() tg.MessagesFilterClass{
	"photos":      func() tg.MessagesFilterClass { return &tg.InputMessagesFilterPhotos{} },
	"videos":      func() tg.MessagesFilterClass { return &tg.InputMessagesFilterVideo{} },
	"documents":   func() tg.MessagesFilterClass { return &tg.InputMessagesFilterDocument{} },
	"links":       func() tg.MessagesFilterClass { return &tg.InputMessagesFilterURL{} },
	"voice":       func() tg.MessagesFilterClass { return &tg.InputMessagesFilterVoice{} },
	"music":       func() tg.MessagesFilterClass { return &tg.InputMessagesFilterMusic{} },
	"gif":         func() tg.MessagesFilterClass { return &tg.InputMessagesFilterGif{} },
	"round_video": func() tg.MessagesFilterClass { return &tg.InputMessagesFilterRoundVideo{} },
	"round_voice": func() tg.MessagesFilterClass { return &tg.InputMessagesFilterRoundVoice{} },
}

// searchMessagesOutput mirrors getMessagesOutput but is declared separately
// so schema generation doesn't alias the two tools. The per-message DTO
// shape is identical (both use messageDTO) so downstream tools
// (EditMessage, DeleteMessage, GetMessageContext) can consume either.
// The envelope differs: this one adds Query, NextOffsetID, and PaginationHint
// for search-specific pagination.
type searchMessagesOutput struct {
	ChatID         int64        `json:"chat_id"`
	Query          string       `json:"query"`
	Messages       []messageDTO `json:"messages"`
	Count          int          `json:"count"`
	HasMore        bool         `json:"has_more"`
	NextOffsetID   string       `json:"next_offset_id,omitempty"`
	PaginationHint string       `json:"pagination_hint,omitempty"`
}

// Register adds the SearchMessages tool to the MCP server.
func (h *MessagesSearchHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "SearchMessages",
		Description: "Search messages by substring within a specific chat via Telegram's server-side messages.search. Returns up to `limit` messages (default 50, max 100) sorted newest-first. " +
			"Supports pagination via `offset_id` (copy `next_offset_id` from a previous response), date range via `from_date` / `to_date` (RFC3339; `to_date` is exclusive — pass midnight of the next day to include a full day), sender filtering via `from_sender_id`, and media-type filtering via `media_type`. " +
			"For cross-chat search use SearchMessagesGlobal. For chat discovery by title use SearchChats.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *MessagesSearchHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in SearchMessagesInput) (*mcp.CallToolResult, *searchMessagesOutput, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return errResult("query is required and must be a non-empty string."), nil, nil
	}

	opts := messages.SearchOptions{
		Query: query,
		Limit: clampLimit(in.Limit, 50, 100),
	}

	if in.OffsetID != "" {
		ref, err := ParseMessageRef(in.OffsetID)
		if err != nil {
			return errInvalidMessageID(in.OffsetID, err), nil, nil
		}
		if ref.Scheduled {
			return errResult("offset_id cannot reference a scheduled message; scheduled messages are not searchable via messages.search."), nil, nil
		}
		opts.OffsetID = ref.ID
	}

	if in.TopMsgID != "" {
		ref, err := ParseMessageRef(in.TopMsgID)
		if err != nil {
			return errResult(fmt.Sprintf("invalid top_msg_id %q: %v. Expected an opaque regular-message handle (e.g. \"42\") pointing at the thread/topic root.", in.TopMsgID, err)), nil, nil
		}
		if ref.Scheduled {
			return errResult("top_msg_id cannot reference a scheduled message; scheduled messages cannot be thread roots."), nil, nil
		}
		opts.TopMsgID = ref.ID
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

	if in.MediaType != "" {
		ctor, ok := mediaFilterMap[in.MediaType]
		if !ok {
			return errResult(fmt.Sprintf("invalid media_type %q: expected one of %s.", in.MediaType, validMediaTypesList())), nil, nil
		}
		opts.Filter = ctor()
	}

	opts.FromSenderID = in.FromSenderID

	result, err := h.provider.Search(ctx, in.ChatID, opts)
	if err != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "SearchMessages", map[string]any{
			"action":  "provider_search_failed",
			"chat_id": in.ChatID,
			"error":   err.Error(),
		})
		return errResult(fmt.Sprintf("Failed to search messages: %v", err)), nil, nil
	}

	out := &searchMessagesOutput{
		ChatID:   in.ChatID,
		Query:    query,
		Messages: make([]messageDTO, 0, len(result.Messages)),
		Count:    result.Count,
		HasMore:  result.HasMore,
	}

	for _, m := range result.Messages {
		out.Messages = append(out.Messages, toMessageDTO(m, false))
	}

	if result.HasMore && result.NextID > 0 {
		out.NextOffsetID = FormatRegularRef(result.NextID)
		out.PaginationHint = fmt.Sprintf("More matches available. Call SearchMessages again with offset_id=%q to fetch the next page.", out.NextOffsetID)
	}

	return nil, out, nil
}

// validMediaTypesList returns the accepted media_type values in a stable
// alphabetical order so error messages don't thrash between calls.
func validMediaTypesList() string {
	keys := make([]string, 0, len(mediaFilterMap))
	for k := range mediaFilterMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
