package tgdata

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// GetChatInfo retrieves detailed information about a specific chat
func GetChatInfo(ctx context.Context, client *tg.Client, chatID int64) (*ChatFullInfo, error) {
	peer, err := tgclient.ResolvePeer(ctx, client, chatID)
	if err != nil {
		return nil, fmt.Errorf("resolving peer: %w", err)
	}

	var info ChatFullInfo
	info.ID = chatID
	now := time.Now().Unix()

	switch p := peer.(type) {
	case *tg.InputPeerUser:
		info.Type = ChatTypeUser
		fullUser, err := client.UsersGetFullUser(ctx, &tg.InputUser{
			UserID:     p.UserID,
			AccessHash: p.AccessHash,
		})
		if err != nil {
			return nil, fmt.Errorf("getting full user info: %w", err)
		}
		for _, u := range fullUser.Users {
			if user, ok := u.(*tg.User); ok && user.ID == p.UserID {
				info.Name = tgclient.UserName(user)
				info.Username = user.Username
				if user.Bot {
					info.Type = ChatTypeBot
				}
				break
			}
		}
		if info.Name == "" {
			return nil, fmt.Errorf("full user info response did not include user %d", p.UserID)
		}
		info.Description = fullUser.FullUser.About

	case *tg.InputPeerChat:
		info.Type = ChatTypeGroup
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
		for _, c := range fullChat.Chats {
			if chat, ok := c.(*tg.Chat); ok {
				info.Name = chat.Title
				break
			}
		}
		if info.Name == "" {
			return nil, fmt.Errorf("full chat info response did not include chat %d", p.ChatID)
		}

	case *tg.InputPeerChannel:
		info.Type = ChatTypeChannel
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
			if channel, ok := c.(*tg.Channel); ok {
				info.Name = channel.Title
				info.Username = channel.Username
				if channel.Megagroup {
					info.Type = ChatTypeSupergroup
				}
				break
			}
		}
		if info.Name == "" {
			return nil, fmt.Errorf("full channel info response did not include channel %d", p.ChannelID)
		}
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
