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
		var chatType ChatType

		users := dlg.Entities.Users()
		chats := dlg.Entities.Chats()
		channels := dlg.Entities.Channels()

		switch p := dlg.Peer.(type) {
		case *tg.InputPeerUser:
			id = p.UserID
			chatType = ChatTypeUser
			if user, ok := users[p.UserID]; ok {
				name = tgclient.UserName(user)
				username = user.Username
				if user.Bot {
					chatType = ChatTypeBot
				}
			}
		case *tg.InputPeerChat:
			id = p.ChatID
			chatType = ChatTypeGroup
			if chat, ok := chats[p.ChatID]; ok {
				name = chat.Title
			}
		case *tg.InputPeerChannel:
			// Convert to user-facing format with -100 prefix
			id = -tgclient.ChannelIDPrefix - p.ChannelID
			chatType = ChatTypeChannel
			if channel, ok := channels[p.ChannelID]; ok {
				name = channel.Title
				username = channel.Username
				if channel.Megagroup {
					chatType = ChatTypeSupergroup
				}
			}
		}

		if name == "" {
			name = "Unknown"
		}

		muted := dialog.NotifySettings.MuteUntil > int(startTime.Unix())
		archived := dialog.FolderID != 0
		if dlg.Deleted() {
			return nil
		}

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

// GetPinnedChats retrieves only pinned chats using Telegram's dedicated
// messages.getPinnedDialogs API. This is one API call regardless of how many
// total dialogs the user has, vs. the old implementation which paginated
// through ALL dialogs (1000+) and filtered client-side. The old approach
// could take 30+ seconds under flood-wait conditions.
func GetPinnedChats(ctx context.Context, client *tg.Client) ([]ChatInfo, error) {
	// folderid=0 is the default folder ("All chats"). Pinned dialogs in
	// archived folders are uncommon enough to ignore for resource listing.
	result, err := client.MessagesGetPinnedDialogs(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("getting pinned dialogs: %w", err)
	}

	// Index users/chats/channels by ID so we can fill in titles for each
	// pinned dialog.
	users := make(map[int64]*tg.User, len(result.Users))
	for _, u := range result.Users {
		if user, ok := u.(*tg.User); ok {
			users[user.ID] = user
		}
	}
	chats := make(map[int64]*tg.Chat, len(result.Chats))
	channels := make(map[int64]*tg.Channel, len(result.Chats))
	for _, c := range result.Chats {
		switch v := c.(type) {
		case *tg.Chat:
			chats[v.ID] = v
		case *tg.Channel:
			channels[v.ID] = v
		}
	}

	startTime := time.Now()
	pinned := make([]ChatInfo, 0, len(result.Dialogs))
	for _, d := range result.Dialogs {
		dialog, ok := d.(*tg.Dialog)
		if !ok || !dialog.Pinned {
			continue
		}

		var info ChatInfo
		info.UnreadCount = dialog.UnreadCount
		info.MentionCount = dialog.UnreadMentionsCount
		info.Muted = dialog.NotifySettings.MuteUntil > int(startTime.Unix())
		info.Pinned = true
		info.Archived = dialog.FolderID != 0

		switch peer := dialog.Peer.(type) {
		case *tg.PeerUser:
			info.ID = peer.UserID
			info.Type = "user"
			if user, ok := users[peer.UserID]; ok {
				info.Name = tgclient.UserName(user)
				info.Username = user.Username
				if user.Bot {
					info.Type = "bot"
				}
			}
		case *tg.PeerChat:
			info.ID = peer.ChatID
			info.Type = "group"
			if chat, ok := chats[peer.ChatID]; ok {
				info.Name = chat.Title
			}
		case *tg.PeerChannel:
			info.ID = -tgclient.ChannelIDPrefix - peer.ChannelID
			info.Type = "channel"
			if channel, ok := channels[peer.ChannelID]; ok {
				info.Name = channel.Title
				info.Username = channel.Username
				if channel.Megagroup {
					info.Type = "supergroup"
				}
			}
		default:
			continue
		}

		if info.Name == "" {
			info.Name = "Unknown"
		}
		pinned = append(pinned, info)
	}

	return pinned, nil
}
