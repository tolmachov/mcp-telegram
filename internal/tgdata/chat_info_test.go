package tgdata

import (
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chatInfoInvoker fakes the MTProto calls GetChatInfo makes for a channel:
// ResolvePeer's probes (users.getUsers → channels.getChannels), then
// channels.getFullChannel and messages.getPeerDialogs. Only fullChannelChats is
// configurable — the entity list channels.getFullChannel echoes back; every
// other call returns a fixed benign "not found"/empty value.
type chatInfoInvoker struct {
	fullChannelChats []tg.ChatClass
}

func (f chatInfoInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch input.(type) {
	case *tg.UsersGetUsersRequest:
		// Not a user — fall through to the channel probe.
		output.(*tg.UserClassVector).Elems = []tg.UserClass{&tg.UserEmpty{}}
	case *tg.ChannelsGetChannelsRequest:
		output.(*tg.MessagesChatsBox).Chats = &tg.MessagesChats{Chats: []tg.ChatClass{
			&tg.Channel{ID: 555, AccessHash: 42, Megagroup: true},
		}}
	case *tg.ChannelsGetFullChannelRequest:
		*output.(*tg.MessagesChatFull) = tg.MessagesChatFull{
			FullChat: &tg.ChannelFull{About: "desc", ParticipantsCount: 100},
			Chats:    f.fullChannelChats,
		}
	case *tg.MessagesGetPeerDialogsRequest:
		*output.(*tg.MessagesPeerDialogs) = tg.MessagesPeerDialogs{}
	default:
		return fmt.Errorf("chatInfoInvoker: unexpected request %T", input)
	}
	return nil
}

// TestGetChatInfoMatchesEntityByID guards the migrated-group case: a
// channels.getFullChannel response can carry more than one channel entity (e.g.
// a linked discussion group alongside the named supergroup). GetChatInfo must
// select the entity whose ID matches the resolved peer, not simply the first
// one in the list.
func TestGetChatInfoMatchesEntityByID(t *testing.T) {
	client := tg.NewClient(chatInfoInvoker{
		fullChannelChats: []tg.ChatClass{
			// Decoy first: a different channel that the old "take the first
			// *tg.Channel" logic would have mis-selected.
			&tg.Channel{ID: 999, Title: "Wrong Linked Channel"},
			&tg.Channel{ID: 555, Title: "Correct Supergroup", Megagroup: true, Username: "correct"},
		},
	})

	info, err := GetChatInfo(context.Background(), client, 555)
	require.NoError(t, err)
	assert.Equal(t, int64(555), info.ID, "ID must come from the resolved peer, not the echoed argument")
	assert.Equal(t, "Correct Supergroup", info.Name, "must match the entity by ID, not take the first")
	assert.Equal(t, "correct", info.Username)
	assert.Equal(t, ChatTypeSupergroup, info.Type)
	assert.Equal(t, "desc", info.Description)
	assert.Equal(t, 100, info.MembersCount)
}

// TestGetChatInfoNoMatchingEntityErrors verifies that when the response never
// includes the resolved channel, GetChatInfo fails loudly rather than returning
// a nameless struct.
func TestGetChatInfoNoMatchingEntityErrors(t *testing.T) {
	client := tg.NewClient(chatInfoInvoker{
		fullChannelChats: []tg.ChatClass{
			&tg.Channel{ID: 999, Title: "Only The Wrong One"},
		},
	})

	_, err := GetChatInfo(context.Background(), client, 555)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "555")
}
