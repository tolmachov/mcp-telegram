package tgclient

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
)

// ChannelIDPrefix is the offset Telegram Bot API adds to channel IDs (the -100 prefix).
const ChannelIDPrefix = 1_000_000_000_000

// ResolvePeer resolves a dialog ID to an InputPeerClass.
// Handles users, chats, channels, and supergroups.
//
// Dialog ID formats:
//   - Positive IDs: users or legacy basic chats
//   - Negative IDs: channels or supergroups
//
// Telegram uses different ID formats:
//   - MTProto API uses raw channel ID (e.g., 1234567890)
//   - Bot API / user-facing format adds -100 prefix (e.g., -1001234567890)
//
// This function automatically converts from user-facing format to MTProto format.
func ResolvePeer(ctx context.Context, client *tg.Client, dialogID int64) (tg.InputPeerClass, error) {
	// Positive IDs are either users or legacy basic chats. Telegram's
	// users.getUsers always returns an entry per requested ID: either a
	// *tg.User (real user, possibly without access_hash) or *tg.UserEmpty
	// (the id does not correspond to a reachable user in our session).
	// We treat UserEmpty as "not a user, try basic chat" — but we verify
	// the basic-chat hypothesis via messages.getChats before returning
	// InputPeerChat, so a deleted/banned user ID doesn't get silently
	// mislabeled as a basic chat and later blow up with "PEER_ID_INVALID"
	// inside some downstream tool call.
	if dialogID > 0 {
		users, err := client.UsersGetUsers(ctx, []tg.InputUserClass{
			&tg.InputUser{UserID: dialogID},
		})
		if err != nil {
			return nil, fmt.Errorf("resolving user %d: %w", dialogID, err)
		}
		if len(users) > 0 {
			if user, ok := users[0].(*tg.User); ok {
				if user.AccessHash == 0 {
					// Known user but we have no access_hash — the current
					// session doesn't share a dialog with them, so there's
					// no way to build a valid InputPeerUser. Fail loudly.
					return nil, fmt.Errorf("user %d resolved but missing access_hash; no shared dialog", dialogID)
				}
				return &tg.InputPeerUser{
					UserID:     dialogID,
					AccessHash: user.AccessHash,
				}, nil
			}
		}

		// Not a user — verify it's actually a legacy basic chat via
		// messages.getChats. If neither users.getUsers nor messages.getChats
		// recognize the ID, return an explicit error rather than fabricating
		// an InputPeerChat that would fail cryptically at the next API call.
		chats, err := client.MessagesGetChats(ctx, []int64{dialogID})
		if err != nil {
			return nil, fmt.Errorf("id %d is not a reachable user (got %T) and verifying as basic chat failed: %w", dialogID, firstUser(users), err)
		}
		if msgChats, ok := chats.(*tg.MessagesChats); ok && len(msgChats.Chats) > 0 {
			if _, ok := msgChats.Chats[0].(*tg.Chat); ok {
				return &tg.InputPeerChat{ChatID: dialogID}, nil
			}
		}
		return nil, fmt.Errorf("id %d is neither a reachable user nor a known basic chat; pass a -100-prefixed id for channels/supergroups, or verify the id with ResolveUsername/SearchChats", dialogID)
	}

	// Negative IDs are channels or supergroups.
	// Convert from user-facing format to MTProto format.
	channelID := -dialogID // Remove minus sign: -(-1001234567890) = 1001234567890
	if channelID > ChannelIDPrefix {
		// Has -100 prefix, remove it: 1001234567890 - 1000000000000 = 1234567890
		channelID -= ChannelIDPrefix
	}

	channels, err := client.ChannelsGetChannels(ctx, []tg.InputChannelClass{
		&tg.InputChannel{ChannelID: channelID},
	})
	if err != nil {
		return nil, fmt.Errorf("resolving channel %d: %w", channelID, err)
	}

	if chats, ok := channels.(*tg.MessagesChats); ok && len(chats.Chats) > 0 {
		if channel, ok := chats.Chats[0].(*tg.Channel); ok {
			return &tg.InputPeerChannel{
				ChannelID:  channel.ID,
				AccessHash: channel.AccessHash,
			}, nil
		}
	}

	return nil, fmt.Errorf("channel %d not found", channelID)
}

// firstUser returns the first element of a users.getUsers response or nil,
// used only to embed the concrete type name (e.g. *tg.UserEmpty) in error
// messages for the "neither user nor chat" fallback path.
func firstUser(users []tg.UserClass) tg.UserClass {
	if len(users) == 0 {
		return nil
	}
	return users[0]
}
