package tools

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
)

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

// Prepared pages keep the immutable query and its provider options together.
// Tool handlers consume these values without restoring cursor fields into
// their original input. Each mode keeps its own reading and presentation.
type historyPage struct {
	chatID           int64
	options          messages.FetchOptions
	includeScheduled bool
	cursor           messagePageCursor
}

type searchPage struct {
	chatID  int64
	options messages.SearchOptions
	cursor  messagePageCursor
}

type repliesPage struct {
	chatID  int64
	rootID  int
	options messages.FetchOptions
	cursor  messagePageCursor
}

func prepareHistoryPage(in GetMessagesInput) (historyPage, *mcp.CallToolResult) {
	other := in
	other.Cursor = ""
	state, invalid := preparePageCursor(in.Cursor, other != (GetMessagesInput{}), messagePageCursor{
		Kind: cursorKindHistory, ChatID: in.ChatID, Limit: in.Limit,
		FromDate: in.FromDate, ToDate: in.ToDate, UnreadOnly: in.UnreadOnly, IncludeScheduled: in.IncludeScheduled,
	})
	if invalid != nil {
		return historyPage{}, invalid
	}
	if invalid = state.firstAnchor(in.BeforeMessageID); invalid != nil {
		return historyPage{}, invalid
	}
	opts, invalid := pageFetchOptions(state)
	if invalid != nil {
		return historyPage{}, invalid
	}
	opts.UnreadOnly = state.UnreadOnly
	return historyPage{chatID: state.ChatID, options: opts, includeScheduled: state.IncludeScheduled, cursor: state}, nil
}

func prepareSearchPage(in SearchMessagesInput) (searchPage, *mcp.CallToolResult) {
	other := in
	other.Cursor = ""
	other.Query = strings.TrimSpace(other.Query)
	state, invalid := preparePageCursor(in.Cursor, other != (SearchMessagesInput{}), messagePageCursor{
		Kind: cursorKindSearch, ChatID: in.ChatID, Limit: in.Limit, Query: other.Query,
		FromDate: in.FromDate, ToDate: in.ToDate, FromSenderID: in.FromSenderID, MediaType: in.MediaType,
	})
	if invalid != nil {
		return searchPage{}, invalid
	}
	if in.Cursor == "" && state.Query == "" {
		return searchPage{}, ErrResult("query is required and must be a non-empty string.")
	}
	if invalid = state.firstAnchor(in.BeforeMessageID); invalid != nil {
		return searchPage{}, invalid
	}
	if in.TopMsgID != "" {
		state.TopMsgID, invalid = parseRegularRef("top_msg_id", in.TopMsgID, "search the thread of")
		if invalid != nil {
			return searchPage{}, invalid
		}
	}
	base, invalid := pageFetchOptions(state)
	if invalid != nil {
		return searchPage{}, invalid
	}
	opts := messages.SearchOptions{
		Query: state.Query, Limit: base.Limit, OffsetID: base.OffsetID, MinDate: base.MinDate, MaxDate: base.MaxDate,
		FromSenderID: state.FromSenderID, TopMsgID: state.TopMsgID,
	}
	if state.MediaType != "" {
		ctor, ok := mediaFilterMap[state.MediaType]
		if !ok {
			return searchPage{}, ErrResult(fmt.Sprintf("invalid media_type %q: expected one of %s.", state.MediaType, strings.Join(mediaTypes, ", ")))
		}
		opts.Filter = ctor()
	}
	return searchPage{chatID: state.ChatID, options: opts, cursor: state}, nil
}

func prepareRepliesPage(in GetRepliesInput) (repliesPage, *mcp.CallToolResult) {
	other := in
	other.Cursor = ""
	state, invalid := preparePageCursor(in.Cursor, other != (GetRepliesInput{}), messagePageCursor{
		Kind: cursorKindReplies, ChatID: in.ChatID, Limit: in.Limit, FromDate: in.FromDate, ToDate: in.ToDate,
	})
	if invalid != nil {
		return repliesPage{}, invalid
	}
	if in.Cursor == "" {
		state.RootMessageID, invalid = parseRegularRef("message_id", in.MessageID, "read replies to")
		if invalid != nil {
			return repliesPage{}, invalid
		}
	}
	if invalid = state.firstAnchor(in.BeforeMessageID); invalid != nil {
		return repliesPage{}, invalid
	}
	opts, invalid := pageFetchOptions(state)
	if invalid != nil {
		return repliesPage{}, invalid
	}
	return repliesPage{chatID: state.ChatID, rootID: state.RootMessageID, options: opts, cursor: state}, nil
}

// preparePageCursor selects either the initial query or the cursor's entire
// query. It never merges them: continuation accepts the cursor alone.
func preparePageCursor(raw string, hasOtherFields bool, first messagePageCursor) (messagePageCursor, *mcp.CallToolResult) {
	if raw != "" {
		if hasOtherFields {
			return messagePageCursor{}, ErrResult("cursor is incompatible with every other field; pass the cursor alone")
		}
		state, err := parseMessagePageCursor(raw, first.Kind)
		if err != nil {
			return messagePageCursor{}, ErrResult(fmt.Sprintf("invalid cursor: %v", err))
		}
		return state, nil
	}
	if first.ChatID == 0 {
		return messagePageCursor{}, errChatIDRequired()
	}
	first.Limit = clampLimit(first.Limit, messages.DefaultFetchOptions().Limit, 100)
	return first, nil
}

// firstAnchor applies only an explicit first-page anchor. A continuation
// cannot supply one, so its raw Telegram offset remains unchanged.
func (c *messagePageCursor) firstAnchor(before string) *mcp.CallToolResult {
	if before == "" {
		return nil
	}
	offset, invalid := parseRegularRef("before_message_id", before, "page before")
	if invalid == nil {
		c.OffsetID = offset
	}
	return invalid
}

func pageFetchOptions(state messagePageCursor) (messages.FetchOptions, *mcp.CallToolResult) {
	minDate, maxDate, invalid := parseDateWindow(state.FromDate, state.ToDate)
	return messages.FetchOptions{Limit: state.Limit, OffsetID: state.OffsetID, MinDate: minDate, MaxDate: maxDate}, invalid
}

func (p historyPage) next(result *messages.FetchResult) (string, string) {
	return pageCursorResult(result, p.cursor, "GetMessages", "messages")
}

func (p searchPage) next(result *messages.FetchResult) (string, string) {
	return pageCursorResult(result, p.cursor, "SearchMessages", "matches")
}

func (p repliesPage) next(result *messages.FetchResult) (string, string) {
	return pageCursorResult(result, p.cursor, "GetReplies", "replies")
}
