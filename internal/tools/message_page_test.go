package tools

import (
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/messages"
)

func TestPreparedHistoryPageKeepsCursorContract(t *testing.T) {
	const fixture = "eyJ2IjoxLCJrIjoiaGlzdG9yeSIsImMiOjc3LCJvIjoyMCwibCI6MiwiZiI6IjE5NzAtMDEtMDFUMDA6MDE6NDBaIiwidCI6IjE5NzAtMDEtMDFUMDA6MDM6MjBaIiwidSI6dHJ1ZSwicyI6dHJ1ZX0"
	first, invalid := prepareHistoryPage(GetMessagesInput{
		ChatID: 77, Limit: 2, BeforeMessageID: "30", FromDate: "1970-01-01T00:01:40Z", ToDate: "1970-01-01T00:03:20Z", UnreadOnly: true, IncludeScheduled: true,
	})
	require.Nil(t, invalid)
	assert.Equal(t, 30, first.options.OffsetID)
	next, hint := first.next(&messages.FetchResult{HasMore: true, NextID: 20})
	assert.Equal(t, fixture, next, "internal preparation must not change the v1 cursor wire format")
	assert.Contains(t, hint, "GetMessages")
	continued, invalid := prepareHistoryPage(GetMessagesInput{Cursor: fixture})
	require.Nil(t, invalid)
	assert.Equal(t, int64(77), continued.chatID)
	assert.True(t, continued.includeScheduled)
	assert.True(t, continued.options.UnreadOnly)
	assert.Equal(t, 2, continued.options.Limit)
	assert.Equal(t, 20, continued.options.OffsetID)
	assert.Equal(t, int64(100), continued.options.MinDate.Unix())
	assert.Equal(t, int64(200), continued.options.MaxDate.Unix())
	for _, result := range []*messages.FetchResult{{HasMore: false, NextID: 20}, {HasMore: true}} {
		cursor, hint := continued.next(result)
		assert.Empty(t, cursor)
		assert.Empty(t, hint)
	}
	_, invalid = prepareHistoryPage(GetMessagesInput{Cursor: fixture, IncludeScheduled: true})
	require.NotNil(t, invalid)
	assert.Contains(t, ResultText(invalid), "cursor alone")
}

func TestPreparedSearchPageRestoresAllFilters(t *testing.T) {
	first, invalid := prepareSearchPage(SearchMessagesInput{
		ChatID: 77, Query: " release ", Limit: 2, BeforeMessageID: "30", FromDate: "1970-01-01T00:01:40Z", ToDate: "1970-01-01T00:03:20Z", FromSenderID: 88, MediaType: "photos", TopMsgID: "7",
	})
	require.Nil(t, invalid)
	cursor, _ := first.next(&messages.FetchResult{HasMore: true, NextID: 20})
	continued, invalid := prepareSearchPage(SearchMessagesInput{Cursor: cursor, Query: "   "})
	require.Nil(t, invalid, "whitespace-only query retains the existing cursor-only behaviour")
	assert.Equal(t, int64(77), continued.chatID)
	assert.Equal(t, messages.SearchOptions{
		Query: "release", Limit: 2, OffsetID: 20, MinDate: first.options.MinDate, MaxDate: first.options.MaxDate, FromSenderID: 88, Filter: &tg.InputMessagesFilterPhotos{}, TopMsgID: 7,
	}, continued.options)
	assert.Equal(t, int64(100), continued.options.MinDate.Unix())
	assert.Equal(t, int64(200), continued.options.MaxDate.Unix())
	_, invalid = prepareSearchPage(SearchMessagesInput{Cursor: cursor, TopMsgID: "7"})
	require.NotNil(t, invalid)
	_, invalid = prepareRepliesPage(GetRepliesInput{Cursor: cursor})
	require.NotNil(t, invalid)
	assert.Contains(t, ResultText(invalid), "not replies")
}

func TestPreparedRepliesPageRestoresThread(t *testing.T) {
	first, invalid := prepareRepliesPage(GetRepliesInput{ChatID: 77, MessageID: "7", BeforeMessageID: "30", Limit: 2, FromDate: "1970-01-01T00:01:40Z", ToDate: "1970-01-01T00:03:20Z"})
	require.Nil(t, invalid)
	cursor, hint := first.next(&messages.FetchResult{HasMore: true, NextID: 20})
	assert.Contains(t, hint, "GetReplies")
	continued, invalid := prepareRepliesPage(GetRepliesInput{Cursor: cursor})
	require.Nil(t, invalid)
	assert.Equal(t, int64(77), continued.chatID)
	assert.Equal(t, 7, continued.rootID)
	expected := first.options
	expected.OffsetID = 20
	assert.Equal(t, expected, continued.options)
	_, invalid = prepareRepliesPage(GetRepliesInput{Cursor: cursor, BeforeMessageID: "30"})
	require.NotNil(t, invalid)
}
