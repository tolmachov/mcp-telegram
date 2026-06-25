package completion

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

func newTestCompleter(chats []tgdata.ChatInfo, listErr error) (*completer, *int) {
	calls := 0
	return &completer{
		list: func(context.Context) ([]tgdata.ChatInfo, error) {
			calls++
			if listErr != nil {
				return nil, listErr
			}
			return chats, nil
		},
		now: func() time.Time { return time.Unix(0, 0) },
	}, &calls
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
	c, _ := newTestCompleter(nil, nil)

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
	c, _ := newTestCompleter(chats, nil)

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
	c, _ := newTestCompleter(chats, nil)

	// Resource-template variable resolves to a numeric id.
	assert.Equal(t, []string{"222"}, complete(t, c, "chat_id", "work"))

	// Matching by the raw id works because the id is folded into the label.
	assert.Equal(t, []string{"111"}, complete(t, c, "chat_id", "111"))
}

func TestChatCompletionSwallowsListError(t *testing.T) {
	c, _ := newTestCompleter(nil, errors.New("flood wait"))
	assert.Empty(t, complete(t, c, "chat", "x"))
}

func TestUnknownArgumentReturnsEmpty(t *testing.T) {
	c, calls := newTestCompleter([]tgdata.ChatInfo{{ID: 1, Name: "Alice"}}, nil)
	assert.Empty(t, complete(t, c, "something_else", "a"))
	assert.Zero(t, *calls, "unknown argument must not fetch chats")
}

func TestChatListIsCachedWithinTTL(t *testing.T) {
	c, calls := newTestCompleter([]tgdata.ChatInfo{{ID: 1, Name: "Alice"}}, nil)
	complete(t, c, "chat", "a")
	complete(t, c, "chat", "al")
	assert.Equal(t, 1, *calls, "repeated completions within the TTL must reuse the cache")
}
