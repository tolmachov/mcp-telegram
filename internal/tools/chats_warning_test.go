package tools

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// TestPageFromTruncatedWarning pins that pageFrom surfaces the truncation
// warning exactly when the snapshot is truncated, and stays silent otherwise.
func TestPageFromTruncatedWarning(t *testing.T) {
	h := &ChatsGetHandler{}
	chats := makeChats(5)

	out := h.pageFrom(chats, 42, 0, 10, true)
	assert.Equal(t, truncatedChatsWarning, out.Warning, "truncated snapshot must warn")

	out = h.pageFrom(chats, 42, 0, 10, false)
	assert.Empty(t, out.Warning, "complete snapshot must not warn")
}

// TestChatsGetCursorPageCarriesTruncationWarning covers the snapshot() path:
// a cursor page served from a truncated cache must repeat the warning so a
// paginating model sees it on every hop, not just the first.
func TestChatsGetCursorPageCarriesTruncationWarning(t *testing.T) {
	const sid int64 = 7
	h := &ChatsGetHandler{cache: &ChatsCache{
		chats:     makeChats(20),
		sessionID: sid,
		truncated: true,
	}}

	cursor := FormatChatsCursor(sid, 3)
	errRes, out, err := h.handleWithCursor(cursor, 5)
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.NotNil(t, out)
	assert.Equal(t, truncatedChatsWarning, out.Warning)
}

// TestSearchChatsTruncationWarning covers the SearchChats side: a search over a
// truncated snapshot must warn that a chat's absence isn't proof it doesn't
// exist. Limit=1 with a matching local chat fills the result set, so the global
// search (which would touch the nil client) is skipped.
func TestSearchChatsTruncationWarning(t *testing.T) {
	h := &ChatsSearchHandler{cache: &ChatsCache{
		chats:     []tgdata.ChatInfo{{ID: 1, Name: "alpha", Type: tgdata.ChatTypeGroup}},
		sessionID: 9,
		truncated: true,
	}}

	errRes, out, err := h.handle(context.Background(), &mcp.CallToolRequest{}, SearchChatsInput{
		Query: "alpha",
		Limit: 1,
	})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.NotNil(t, out)
	require.Len(t, out.Results, 1, "local match should fill the single slot and skip global search")
	assert.Contains(t, out.Warning, truncatedChatsWarning)
}
