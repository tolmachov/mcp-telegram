package tgdata

import (
	"github.com/gotd/td/tg"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// UserType classifies a user entity as a person or a bot.
func UserType(u *tg.User) ChatType {
	if u.Bot {
		return ChatTypeBot
	}
	return ChatTypeUser
}

// ChannelType classifies a channel entity as a broadcast channel or a
// supergroup (a megagroup, which MTProto models as a channel).
func ChannelType(c *tg.Channel) ChatType {
	if c.Megagroup {
		return ChatTypeSupergroup
	}
	return ChatTypeChannel
}

// ChatInfoFromUser builds the identity part of ChatInfo (ID, type, name,
// username) for a user entity.
func ChatInfoFromUser(u *tg.User) ChatInfo {
	return ChatInfo{
		ID:       u.ID,
		Type:     UserType(u),
		Name:     tgclient.UserName(u),
		Username: u.Username,
	}
}

// ChatInfoFromChat builds the identity part of ChatInfo for a basic group or
// channel entity. ok is false for the placeholder forms (empty, forbidden)
// that carry no usable chat.
func ChatInfoFromChat(c tg.ChatClass) (ChatInfo, bool) {
	switch v := c.(type) {
	case *tg.Chat:
		return basicGroupInfo(v), true
	case *tg.Channel:
		return channelInfo(v), true
	default:
		return ChatInfo{}, false
	}
}

func basicGroupInfo(c *tg.Chat) ChatInfo {
	return ChatInfo{ID: c.ID, Type: ChatTypeGroup, Name: c.Title}
}

func channelInfo(c *tg.Channel) ChatInfo {
	return ChatInfo{ID: c.ID, Type: ChannelType(c), Name: c.Title, Username: c.Username}
}
