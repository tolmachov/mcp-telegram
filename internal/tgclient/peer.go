package tgclient

import (
	"context"

	"github.com/gotd/td/tg"
)

// ResolvePeer resolves a dialog ID to an InputPeerClass.
// Handles users, chats, channels, and supergroups.
//
// Dialog ID formats:
//   - Positive IDs: users or basic chats
//   - Negative IDs: channels or supergroups
//
// Telegram uses different ID formats:
//   - MTProto API uses raw channel ID (e.g., 1234567890)
//   - Bot API / user-facing format adds -100 prefix (e.g., -1001234567890)
//
// This function automatically converts from user-facing format to MTProto format.
func ResolvePeer(ctx context.Context, client *tg.Client, dialogID int64) (tg.InputPeerClass, error) {
	// Try as user first
	if dialogID > 0 {
		users, err := client.UsersGetUsers(ctx, []tg.InputUserClass{
			&tg.InputUser{UserID: dialogID},
		})
		if err == nil && len(users) > 0 {
			if user, ok := users[0].(*tg.User); ok && user.AccessHash != 0 {
				return &tg.InputPeerUser{
					UserID:     dialogID,
					AccessHash: user.AccessHash,
				}, nil
			}
		}

		return &tg.InputPeerChat{ChatID: dialogID}, nil
	}

	// Negative IDs are channels or supergroups.
	// Convert from user-facing format to MTProto format.
	channelID := -dialogID // Remove minus sign: -(-1001234567890) = 1001234567890
	if channelID > 1000000000000 {
		// Has -100 prefix, remove it: 1001234567890 - 1000000000000 = 1234567890
		channelID -= 1000000000000
	}

	channels, err := client.ChannelsGetChannels(ctx, []tg.InputChannelClass{
		&tg.InputChannel{ChannelID: channelID},
	})
	if err != nil {
		return &tg.InputPeerChannel{ChannelID: channelID}, nil
	}

	if chats, ok := channels.(*tg.MessagesChats); ok && len(chats.Chats) > 0 {
		if channel, ok := chats.Chats[0].(*tg.Channel); ok {
			return &tg.InputPeerChannel{
				ChannelID:  channel.ID,
				AccessHash: channel.AccessHash,
			}, nil
		}
	}

	return &tg.InputPeerChannel{ChannelID: channelID}, nil
}
