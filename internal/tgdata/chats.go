package tgdata

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/tg"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// ProgressFunc is a callback for reporting progress
type ProgressFunc func(current int, message string)

// GetChats retrieves a list of all chats
func GetChats(ctx context.Context, client *tg.Client, onProgress ProgressFunc) (*ChatsList, error) {
	var chatsList []ChatInfo

	startTime := time.Now()
	chatCount := 0

	err := query.GetDialogs(client).BatchSize(100).ForEach(ctx, func(ctx context.Context, dlg dialogs.Elem) error {
		chatCount++
		if chatCount%100 == 0 && onProgress != nil {
			onProgress(chatCount, fmt.Sprintf("Processed %d chats...", chatCount))
		}

		dialog, ok := dlg.Dialog.(*tg.Dialog)
		if !ok {
			return nil
		}

		var name string
		var username string
		var id int64
		var chatType string

		users := dlg.Entities.Users()
		chats := dlg.Entities.Chats()
		channels := dlg.Entities.Channels()

		switch p := dlg.Peer.(type) {
		case *tg.InputPeerUser:
			id = p.UserID
			chatType = "user"
			if user, ok := users[p.UserID]; ok {
				name = tgclient.UserName(user)
				username = user.Username
				if user.Bot {
					chatType = "bot"
				}
			}
		case *tg.InputPeerChat:
			id = p.ChatID
			chatType = "group"
			if chat, ok := chats[p.ChatID]; ok {
				name = chat.Title
			}
		case *tg.InputPeerChannel:
			// Convert to user-facing format with -100 prefix
			id = -1000000000000 - p.ChannelID
			chatType = "channel"
			if channel, ok := channels[p.ChannelID]; ok {
				name = channel.Title
				username = channel.Username
				if channel.Megagroup {
					chatType = "supergroup"
				}
			}
		}

		if name == "" {
			name = "Unknown"
		}

		muted := dialog.NotifySettings.MuteUntil > int(startTime.Unix())
		archived := dialog.FolderID != 0
		deleted := dlg.Deleted()

		chatsList = append(chatsList, ChatInfo{
			ID:           id,
			Type:         chatType,
			Name:         name,
			Username:     username,
			UnreadCount:  dialog.UnreadCount,
			MentionCount: dialog.UnreadMentionsCount,
			Muted:        muted,
			Pinned:       dialog.Pinned,
			Archived:     archived,
			Deleted:      deleted,
		})

		return nil
	})

	if onProgress != nil {
		onProgress(chatCount, fmt.Sprintf("Finished: %d chats fetched in %v", chatCount, time.Since(startTime)))
	}

	if err != nil {
		return nil, fmt.Errorf("listing chats: %w", err)
	}

	return &ChatsList{
		Chats: chatsList,
		Count: len(chatsList),
	}, nil
}
