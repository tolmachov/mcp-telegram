package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

func TestScoreChatsSearchesTitleUsernameAndID(t *testing.T) {
	chats := []tgdata.ChatInfo{
		{ID: 42, Name: "Unrelated", Username: "orbit_news"},
		{ID: 100, Name: "Orbit Engineering"},
		{ID: 4242, Name: "Another"},
	}

	byUsername := scoreChats("@orbit_news", chats)
	require.Len(t, byUsername, 1)
	assert.Equal(t, int64(42), byUsername[0].ID)
	assert.Equal(t, 0, byUsername[0].Distance)

	byTitle := scoreChats("Orbit Eng", chats)
	require.NotEmpty(t, byTitle)
	assert.Equal(t, int64(100), byTitle[0].ID)

	byID := scoreChats("4242", chats)
	require.Len(t, byID, 1)
	assert.Equal(t, int64(4242), byID[0].ID)
}

func TestMergeSearchResultsUsesUniformBestScore(t *testing.T) {
	local := []SearchResult{{ChatInfo: tgdata.ChatInfo{ID: 1, Name: "local"}, Distance: 600}}
	global := []SearchResult{
		{ChatInfo: tgdata.ChatInfo{ID: 2, Name: "global"}, Distance: 100},
		{ChatInfo: tgdata.ChatInfo{ID: 1, Name: "duplicate"}, Distance: 200},
	}
	got := mergeSearchResults(local, global)
	require.Len(t, got, 2)
	assert.Equal(t, int64(2), got[0].ID)
	assert.Equal(t, int64(1), got[1].ID)
	assert.Equal(t, 200, got[1].Distance)
}
