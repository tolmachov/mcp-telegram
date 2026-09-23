package resources

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

func TestChatsResourceServesSharedSnapshot(t *testing.T) {
	loads := 0
	cache := tgdata.NewChatsCache(func(context.Context, tgdata.ProgressFunc) (*tgdata.ChatsList, error) {
		loads++
		return &tgdata.ChatsList{Chats: []tgdata.ChatInfo{{ID: 1, Name: "Alpha"}}, Count: 1, Truncated: true}, nil
	})
	h := NewChatsHandler(cache)

	for range 2 {
		res, err := h.handle(t.Context(), &mcp.ReadResourceRequest{})
		require.NoError(t, err)
		require.Len(t, res.Contents, 1)
		var got tgdata.ChatsList
		require.NoError(t, json.Unmarshal([]byte(res.Contents[0].Text), &got))
		assert.Equal(t, tgdata.ChatsList{Chats: []tgdata.ChatInfo{{ID: 1, Name: "Alpha"}}, Count: 1, Truncated: true}, got)
	}
	assert.Equal(t, 1, loads, "repeated reads must reuse the cached snapshot")
}
