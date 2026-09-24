package completion

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// newTestCompleter returns a completer over a real chat cache whose loader
// serves chats (or fails with listErr), and the count of its loads.
func newTestCompleter(t *testing.T, chats []tgdata.ChatInfo, listErr error) (*completer, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	cache := tgdata.NewChatsCache(t.Context(), func(context.Context, tgdata.ProgressFunc) (*tgdata.ChatsList, error) {
		calls.Add(1)
		if listErr != nil {
			return nil, listErr
		}
		return &tgdata.ChatsList{Chats: chats, Count: len(chats)}, nil
	})
	return &completer{chats: cache}, &calls
}

func complete(t *testing.T, c *completer, name, value string) []string {
	t.Helper()
	res, err := c.handle(context.Background(), &mcp.CompleteRequest{
		Params: &mcp.CompleteParams{
			Argument: mcp.CompleteParamsArgument{Name: name, Value: value},
			Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "chat-catchup"},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotNil(t, res.Completion.Values) // must never be JSON null
	return res.Completion.Values
}

func TestPeriodCompletion(t *testing.T) {
	c, _ := newTestCompleter(t, nil, nil)

	assert.Equal(t, []string{"day", "week", "month"}, complete(t, c, "period", ""))
	assert.Equal(t, []string{"week"}, complete(t, c, "period", "w"))
	assert.Empty(t, complete(t, c, "period", "zzz"))
}

func TestChatCompletionByName(t *testing.T) {
	chats := []tgdata.ChatInfo{
		{ID: 1, Name: "Alice", Username: "alice_tg"},
		{ID: 2, Name: "Work Team"},
		{ID: 3, Name: "Bob"},
	}
	c, _ := newTestCompleter(t, chats, nil)

	// Empty query offers everything, username preferred over title.
	assert.Equal(t, []string{"@alice_tg", "Work Team", "Bob"}, complete(t, c, "chat", ""))

	// Fuzzy match on title.
	assert.Equal(t, []string{"Work Team"}, complete(t, c, "chat", "work"))

	// Match on the @username.
	assert.Equal(t, []string{"@alice_tg"}, complete(t, c, "chat", "alice"))
}

func TestChatIDCompletionReturnsNumericIDs(t *testing.T) {
	chats := []tgdata.ChatInfo{
		{ID: 111, Name: "Alice", Username: "alice_tg"},
		{ID: 222, Name: "Work Team"},
	}
	c, _ := newTestCompleter(t, chats, nil)

	// Resource-template variable resolves to a numeric id.
	assert.Equal(t, []string{"222"}, complete(t, c, "chat_id", "work"))

	// Matching by the raw id works because the id is folded into the label.
	assert.Equal(t, []string{"111"}, complete(t, c, "chat_id", "111"))
}

func TestChatCompletionSwallowsListError(t *testing.T) {
	c, _ := newTestCompleter(t, nil, errors.New("flood wait"))
	assert.Empty(t, complete(t, c, "chat", "x"))
}

func TestUnknownArgumentReturnsEmpty(t *testing.T) {
	c, calls := newTestCompleter(t, []tgdata.ChatInfo{{ID: 1, Name: "Alice"}}, nil)
	assert.Empty(t, complete(t, c, "something_else", "a"))
	assert.Zero(t, calls.Load(), "unknown argument must not fetch chats")
}

func TestCandidatesAreBuiltOncePerSnapshot(t *testing.T) {
	chats := []tgdata.ChatInfo{{ID: 1, Name: "Alice"}}
	cache := tgdata.NewChatsCache(t.Context(), func(context.Context, tgdata.ProgressFunc) (*tgdata.ChatsList, error) {
		return &tgdata.ChatsList{Chats: chats}, nil
	})
	c := &completer{chats: cache}

	assert.Equal(t, []string{"Alice"}, complete(t, c, "chat", "a"))
	built := c.cands.Load()
	complete(t, c, "chat", "al")
	assert.Same(t, built, c.cands.Load(), "the same snapshot must reuse its candidates")

	chats = []tgdata.ChatInfo{{ID: 2, Name: "Bob"}}
	_, err := cache.Load(t.Context(), nil, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"Bob"}, complete(t, c, "chat", "b"))
	assert.NotSame(t, built, c.cands.Load(), "a new snapshot must rebuild the candidates")
}
