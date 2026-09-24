package telegram

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NotUser answers the resolver's users.getUsers probe for id with "not a
// user", so resolution falls through to the channel probe.
func NotUser(t testing.TB, id int64) InvokeFunc {
	t.Helper()
	return Typed(func(_ context.Context, req *tg.UsersGetUsersRequest, out *tg.UserClassVector) error {
		require.Len(t, req.ID, 1)
		assert.Equal(t, id, req.ID[0].(*tg.InputUser).UserID)
		out.Elems = nil
		return nil
	})
}

// Channel answers the resolver's channels.getChannels probe for id with a
// channel carrying accessHash.
func Channel(t testing.TB, id, accessHash int64) InvokeFunc {
	t.Helper()
	return Typed(func(_ context.Context, req *tg.ChannelsGetChannelsRequest, out *tg.MessagesChatsBox) error {
		require.Len(t, req.ID, 1)
		input, ok := req.ID[0].(*tg.InputChannel)
		require.True(t, ok)
		assert.Equal(t, id, input.ChannelID)
		out.Chats = &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Channel{ID: id, AccessHash: accessHash, Title: "Channel"}}}
		return nil
	})
}
