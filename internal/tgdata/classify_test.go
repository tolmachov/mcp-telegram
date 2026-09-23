package tgdata

import (
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatInfoFromUser(t *testing.T) {
	assert.Equal(t, ChatInfo{ID: 1, Type: ChatTypeUser, Name: "@alice", Username: "alice"},
		ChatInfoFromUser(&tg.User{ID: 1, FirstName: "Alice", Username: "alice"}))
	assert.Equal(t, ChatTypeBot, ChatInfoFromUser(&tg.User{ID: 2, Bot: true}).Type)
}

func TestChatInfoFromChat(t *testing.T) {
	group, ok := ChatInfoFromChat(&tg.Chat{ID: 2, Title: "Group"})
	require.True(t, ok)
	assert.Equal(t, ChatInfo{ID: 2, Type: ChatTypeGroup, Name: "Group"}, group)

	channel, ok := ChatInfoFromChat(&tg.Channel{ID: 3, Title: "News", Username: "news"})
	require.True(t, ok)
	assert.Equal(t, ChatInfo{ID: 3, Type: ChatTypeChannel, Name: "News", Username: "news"}, channel)

	super, ok := ChatInfoFromChat(&tg.Channel{ID: 4, Title: "Talk", Megagroup: true})
	require.True(t, ok)
	assert.Equal(t, ChatTypeSupergroup, super.Type)

	_, ok = ChatInfoFromChat(&tg.ChatForbidden{ID: 5})
	assert.False(t, ok)
}
