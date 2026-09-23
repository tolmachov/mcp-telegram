package tools

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/gotd/td/tg"
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
	MediaType       string `json:"media_type,omitempty" jsonschema:"Optional message-type filter. Leave empty for plain text search."`
	TopMsgID        string `json:"top_msg_id,omitempty" jsonschema:"Opaque regular-message handle of a forum topic or reply thread root. When set\\, results are restricted to that thread."`
}

// mediaFilterMap translates the whitelisted media_type enum into its
// corresponding tg.MessagesFilterClass constructor. Its keys are the enum.
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

// mediaTypes is the sorted media_type enum.
var mediaTypes = slices.Sorted(maps.Keys(mediaFilterMap))

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

// Register adds the SearchMessages tool to the MCP server.
func (h *MessagesSearchHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name: "SearchMessages",
		Description: "Search messages by substring within a specific chat via Telegram's server-side messages.search. Returns up to `limit` messages (default 50, max 100) sorted newest-first. " +
			"Continue with `cursor` alone; it embeds the original query and filters. Use `before_message_id` only for the initial anchor. Date range uses inclusive `from_date` and exclusive `to_date`. " +
			"For cross-chat search use SearchMessagesGlobal. For chat discovery by title use SearchChats.",
		InputSchema: inputSchemaWithEnums[SearchMessagesInput](map[string][]string{
			"media_type": mediaTypes,
		}),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *MessagesSearchHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in SearchMessagesInput) (*mcp.CallToolResult, *searchMessagesOutput, error) {
	state := messagePageCursor{Kind: cursorKindSearch}
	query := strings.TrimSpace(in.Query)
	if in.Cursor != "" {
		if in.ChatID != 0 || query != "" || in.Limit != 0 || in.BeforeMessageID != "" || in.FromDate != "" || in.ToDate != "" || in.FromSenderID != 0 || in.MediaType != "" || in.TopMsgID != "" {
			return errResult("cursor is incompatible with every other field; pass the cursor alone"), nil, nil
		}
		var err error
		state, err = parseMessagePageCursor(in.Cursor, cursorKindSearch)
		if err != nil {
			return errResult(fmt.Sprintf("invalid cursor: %v", err)), nil, nil
		}
		in.ChatID, in.Limit = state.ChatID, state.Limit
		in.FromDate, in.ToDate = state.FromDate, state.ToDate
		in.FromSenderID, in.MediaType = state.FromSenderID, state.MediaType
		query = state.Query
	} else {
		if in.ChatID == 0 {
			return errChatIDRequired(), nil, nil
		}
		if query == "" {
			return errResult("query is required and must be a non-empty string."), nil, nil
		}
	}

	opts := messages.SearchOptions{Query: query, Limit: clampLimit(in.Limit, 50, 100)}
	if in.Cursor != "" {
		opts.Limit, opts.OffsetID, opts.TopMsgID = state.Limit, state.OffsetID, state.TopMsgID
	} else if in.BeforeMessageID != "" {
		var errRes *mcp.CallToolResult
		opts.OffsetID, errRes = parseRegularRef("before_message_id", in.BeforeMessageID, "page before")
		if errRes != nil {
			return errRes, nil, nil
		}
	}

	if in.TopMsgID != "" {
		var errRes *mcp.CallToolResult
		opts.TopMsgID, errRes = parseRegularRef("top_msg_id", in.TopMsgID, "search the thread of")
		if errRes != nil {
			return errRes, nil, nil
		}
	}

	var dateErr *mcp.CallToolResult
	opts.MinDate, opts.MaxDate, dateErr = parseDateWindow(in.FromDate, in.ToDate)
	if dateErr != nil {
		return dateErr, nil, nil
	}

	if in.MediaType != "" {
		ctor, ok := mediaFilterMap[in.MediaType]
		if !ok {
			return errResult(fmt.Sprintf("invalid media_type %q: expected one of %s.", in.MediaType, strings.Join(mediaTypes, ", "))), nil, nil
		}
		opts.Filter = ctor()
	}

	opts.FromSenderID = in.FromSenderID
	if in.Cursor == "" {
		state = messagePageCursor{Kind: cursorKindSearch, ChatID: in.ChatID, Limit: opts.Limit, FromDate: in.FromDate, ToDate: in.ToDate, Query: query, FromSenderID: in.FromSenderID, MediaType: in.MediaType, TopMsgID: opts.TopMsgID}
	}

	result, err := h.provider.Search(ctx, in.ChatID, opts)
	if err != nil {
		return nil, nil, failed(fmt.Sprintf("search messages in chat %d", in.ChatID), err)
	}

	out := &searchMessagesOutput{
		ChatID:   in.ChatID,
		Query:    query,
		Messages: presentation.FromMessages(result.Messages, false),
		Count:    result.Count,
		HasMore:  result.HasMore,
	}
	out.NextCursor, out.PaginationHint = pageCursorResult(result, state, "SearchMessages", "matches")

	return nil, out, nil
}
