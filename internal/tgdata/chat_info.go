package tgdata

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// GetChatInfo retrieves detailed information about a specific chat.
func GetChatInfo(ctx context.Context, peers *tgclient.Resolver, chatID int64) (*ChatFullInfo, error) {
	return tgclient.WithPeer(ctx, peers, chatID, nil, nil, func(p tgclient.Peer) (*ChatFullInfo, error) {
		return chatInfo(ctx, peers.Client(), chatID, p.Input)
	})
}

func chatInfo(ctx context.Context, client *tg.Client, chatID int64, peer tg.InputPeerClass) (*ChatFullInfo, error) {
	var info ChatFullInfo
	now := time.Now().Unix()

	switch p := peer.(type) {
	case *tg.InputPeerUser:
		fullUser, err := client.UsersGetFullUser(ctx, &tg.InputUser{
			UserID:     p.UserID,
			AccessHash: p.AccessHash,
		})
		if err != nil {
			return nil, fmt.Errorf("getting full user info: %w", err)
		}
		for _, u := range fullUser.Users {
			if user, ok := u.(*tg.User); ok && user.ID == p.UserID {
				info.ChatInfo = ChatInfoFromUser(user)
				break
			}
		}
		if info.Name == "" {
			return nil, fmt.Errorf("full user info response did not include user %d", p.UserID)
		}
		info.Description = fullUser.FullUser.About

	case *tg.InputPeerChat:
		fullChat, err := client.MessagesGetFullChat(ctx, p.ChatID)
		if err != nil {
			return nil, fmt.Errorf("getting full chat info: %w", err)
		}
		if chat, ok := fullChat.FullChat.(*tg.ChatFull); ok {
			info.Description = chat.About
			if participants, ok := chat.Participants.(*tg.ChatParticipants); ok {
				info.MembersCount = len(participants.Participants)
			}
		}
		// Match by ID: a migrated group returns both the legacy *tg.Chat and the
		// new *tg.Channel in Chats, so taking the first entry could pick the wrong
		// title.
		for _, c := range fullChat.Chats {
			if chat, ok := c.(*tg.Chat); ok && chat.ID == p.ChatID {
				info.ChatInfo = basicGroupInfo(chat)
				break
			}
		}
		if info.Name == "" {
			return nil, fmt.Errorf("full chat info response did not include chat %d", p.ChatID)
		}

	case *tg.InputPeerChannel:
		fullChannel, err := client.ChannelsGetFullChannel(ctx, &tg.InputChannel{
			ChannelID:  p.ChannelID,
			AccessHash: p.AccessHash,
		})
		if err != nil {
			return nil, fmt.Errorf("getting full channel info: %w", err)
		}
		if full, ok := fullChannel.FullChat.(*tg.ChannelFull); ok {
			info.Description = full.About
			info.MembersCount = full.ParticipantsCount
		}
		for _, c := range fullChannel.Chats {
			if channel, ok := c.(*tg.Channel); ok && channel.ID == p.ChannelID {
				info.ChatInfo = channelInfo(channel)
				break
			}
		}
		if info.Name == "" {
			return nil, fmt.Errorf("full channel info response did not include channel %d", p.ChannelID)
		}

	default:
		return nil, fmt.Errorf("unsupported peer type %T for chat %d", peer, chatID)
	}

	// Get chat info for unread count, mute status, etc.
	dialogs, err := client.MessagesGetPeerDialogs(ctx, []tg.InputDialogPeerClass{
		&tg.InputDialogPeer{Peer: peer},
	})
	if err != nil {
		return nil, fmt.Errorf("getting peer dialog info: %w", err)
	}
	if len(dialogs.Dialogs) > 0 {
		if dialog, ok := dialogs.Dialogs[0].(*tg.Dialog); ok {
			info.UnreadCount = dialog.UnreadCount
			info.MentionCount = dialog.UnreadMentionsCount
			info.Muted = dialog.NotifySettings.MuteUntil > int(now)
			info.Pinned = dialog.Pinned
			info.Archived = dialog.FolderID != 0
		}
	}

	return &info, nil
}
