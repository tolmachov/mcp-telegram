package tools

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	telegramfake "github.com/tolmachov/mcp-telegram/internal/testutil/telegram"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// globalSearchPeers returns a resolver whose client answers one
// contacts.search with chats, or with err.
func globalSearchPeers(t *testing.T, chats []tg.ChatClass, err error) (*tgclient.Resolver, *telegramfake.Invoker) {
	t.Helper()
	inv := telegramfake.New(telegramfake.Typed(func(_ context.Context, _ *tg.ContactsSearchRequest, out *tg.ContactsFound) error {
		if err != nil {
			return err
		}
		out.Chats = chats
		return nil
	}))
	return tgclient.NewResolver(t.Context(), tg.NewClient(inv)), inv
}

// TestSearchChatsMergesGlobalResults verifies global matches join the local
// ones and feed the resolver, so their IDs resolve without a probe.
func TestSearchChatsMergesGlobalResults(t *testing.T) {
	cache, _ := seededChatsCache(t, []tgdata.ChatInfo{{ID: 1, Name: "alpha local"}}, false)
	peers, inv := globalSearchPeers(t, []tg.ChatClass{&tg.Channel{ID: 2, AccessHash: 22, Title: "alpha global", Broadcast: true}}, nil)
	h := NewChatsSearchHandler(peers, cache)

	errRes, out, err := h.handle(t.Context(), &mcp.CallToolRequest{}, SearchChatsInput{Query: "alpha"})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.Len(t, out.Results, 2)
	assert.Empty(t, out.Warning)
	_, err = peers.Resolve(t.Context(), 2)
	require.NoError(t, err, "a chat the global search found resolves from the cache")
	assert.Zero(t, inv.Remaining())
}

// TestSearchChatsWarnsOnGlobalFailure verifies a failed global search still
// returns the local matches, with a warning.
func TestSearchChatsWarnsOnGlobalFailure(t *testing.T) {
	cache, _ := seededChatsCache(t, []tgdata.ChatInfo{{ID: 1, Name: "alpha"}}, false)
	peers, _ := globalSearchPeers(t, nil, errors.New("boom"))
	h := NewChatsSearchHandler(peers, cache)

	errRes, out, err := h.handle(t.Context(), &mcp.CallToolRequest{}, SearchChatsInput{Query: "alpha"})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.Len(t, out.Results, 1)
	assert.Contains(t, out.Warning, "Global search failed")
}

// TestSearchChatsSearchesGloballyWhileLoading verifies the global search does
// not wait for the local listing: the loader here only finishes once
// contacts.search has been asked.
func TestSearchChatsSearchesGloballyWhileLoading(t *testing.T) {
	asked := make(chan struct{})
	inv := telegramfake.New(telegramfake.Typed(func(context.Context, *tg.ContactsSearchRequest, *tg.ContactsFound) error {
		close(asked)
		return nil
	}))
	cache := tgdata.NewChatsCache(t.Context(), func(ctx context.Context, _ tgdata.ProgressFunc) (*tgdata.ChatsList, error) {
		select {
		case <-asked:
			return &tgdata.ChatsList{Chats: []tgdata.ChatInfo{{ID: 1, Name: "alpha"}}}, nil
		case <-time.After(10 * time.Second):
			return nil, errors.New("the global search waited for the local listing")
		}
	})
	h := NewChatsSearchHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv)), cache)

	_, out, err := h.handle(t.Context(), &mcp.CallToolRequest{}, SearchChatsInput{Query: "alpha"})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
}

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
