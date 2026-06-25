package tgdata

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// ProgressFunc is a callback for reporting progress
type ProgressFunc func(current int, message string)

// entityMaps indexes the users/chats/channels returned alongside a dialogs
// response so a dialog's peer can be resolved to a title and access hash.
type entityMaps struct {
	users    map[int64]*tg.User
	chats    map[int64]*tg.Chat
	channels map[int64]*tg.Channel
}

// newEntityMaps builds lookup tables from the raw user/chat slices of a
// dialogs response. Channels and legacy chats both arrive in the chats slice
// and are split by concrete type.
func newEntityMaps(users []tg.UserClass, chats []tg.ChatClass) entityMaps {
	em := entityMaps{
		users:    make(map[int64]*tg.User, len(users)),
		chats:    make(map[int64]*tg.Chat),
		channels: make(map[int64]*tg.Channel),
	}
	for _, u := range users {
		if user, ok := u.(*tg.User); ok {
			em.users[user.ID] = user
		}
	}
	for _, c := range chats {
		switch v := c.(type) {
		case *tg.Chat:
			em.chats[v.ID] = v
		case *tg.Channel:
			em.channels[v.ID] = v
		}
	}
	return em
}

// dialogToChatInfo maps a dialog to ChatInfo using the response entities.
// It returns ok=false when the peer's entity is missing from the response
// (a "phantom" dialog for a deleted/left/migrated chat) so the caller can
// skip it — matching the previous behaviour where such dialogs resolved to
// an empty input peer and were dropped.
func (em entityMaps) dialogToChatInfo(dialog *tg.Dialog, now time.Time) (ChatInfo, bool) {
	info := ChatInfo{
		UnreadCount:  dialog.UnreadCount,
		MentionCount: dialog.UnreadMentionsCount,
		Muted:        dialog.NotifySettings.MuteUntil > int(now.Unix()),
		Pinned:       dialog.Pinned,
		Archived:     dialog.FolderID != 0,
	}

	switch peer := dialog.Peer.(type) {
	case *tg.PeerUser:
		info.ID = peer.UserID
		info.Type = ChatTypeUser
		user, ok := em.users[peer.UserID]
		if !ok {
			return ChatInfo{}, false
		}
		info.Name = tgclient.UserName(user)
		info.Username = user.Username
		if user.Bot {
			info.Type = ChatTypeBot
		}
	case *tg.PeerChat:
		info.ID = peer.ChatID
		info.Type = ChatTypeGroup
		chat, ok := em.chats[peer.ChatID]
		if !ok {
			return ChatInfo{}, false
		}
		info.Name = chat.Title
	case *tg.PeerChannel:
		// Convert to user-facing format with -100 prefix
		info.ID = -tgclient.ChannelIDPrefix - peer.ChannelID
		info.Type = ChatTypeChannel
		channel, ok := em.channels[peer.ChannelID]
		if !ok {
			return ChatInfo{}, false
		}
		info.Name = channel.Title
		info.Username = channel.Username
		if channel.Megagroup {
			info.Type = ChatTypeSupergroup
		}
	default:
		return ChatInfo{}, false
	}

	if info.Name == "" {
		info.Name = "Unknown"
	}
	return info, true
}

// offsetPeer builds the input peer used to paginate past the given dialog.
// Crucially, a legacy group (PeerChat) needs only its ID — no access hash —
// so the offset is recoverable even when the chat's entity is absent from the
// response. This is the case the gotd dialog iterator mishandles: it aborts
// the whole listing with "get offset peer: chat <id> not found" instead of
// falling back. Users/channels require an access hash; when their entity is
// missing we fall back to an empty peer rather than fail.
func offsetPeer(p tg.PeerClass, em entityMaps) tg.InputPeerClass {
	switch v := p.(type) {
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: v.ChatID}
	case *tg.PeerUser:
		if u, ok := em.users[v.UserID]; ok {
			return &tg.InputPeerUser{UserID: v.UserID, AccessHash: u.AccessHash}
		}
	case *tg.PeerChannel:
		if c, ok := em.channels[v.ChannelID]; ok {
			return &tg.InputPeerChannel{ChannelID: v.ChannelID, AccessHash: c.AccessHash}
		}
	}
	return &tg.InputPeerEmpty{}
}

// peerLookupKey is a comparable identity for a peer, used to match a dialog to
// its top message within a page.
type peerLookupKey struct {
	kind byte
	id   int64
}

func peerKeyOf(p tg.PeerClass) peerLookupKey {
	switch v := p.(type) {
	case *tg.PeerUser:
		return peerLookupKey{'u', v.UserID}
	case *tg.PeerChat:
		return peerLookupKey{'c', v.ChatID}
	case *tg.PeerChannel:
		return peerLookupKey{'h', v.ChannelID}
	}
	return peerLookupKey{}
}

// GetChats retrieves a list of all chats.
//
// It paginates messages.getDialogs manually rather than via gotd's
// query.GetDialogs helper. The helper computes the next-page offset by
// extracting the last dialog's peer from the response entities and aborts the
// entire listing when that entity is missing (a phantom dialog). Doing it by
// hand lets us recover the offset for legacy groups (which need no access
// hash) and tolerate the rest, so a single phantom dialog can no longer break
// the whole chat list.
func GetChats(ctx context.Context, client *tg.Client, onProgress ProgressFunc) (*ChatsList, error) {
	var chatsList []ChatInfo

	startTime := time.Now()
	chatCount := 0

	var (
		offsetID   int
		offsetDate int
	)
	var offset tg.InputPeerClass = &tg.InputPeerEmpty{}

	for {
		resp, err := client.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetID:   offsetID,
			OffsetDate: offsetDate,
			OffsetPeer: offset,
			Limit:      100,
		})
		if err != nil {
			return nil, fmt.Errorf("listing chats: %w", err)
		}

		var (
			dialogs  []tg.DialogClass
			messages []tg.MessageClass
			users    []tg.UserClass
			chats    []tg.ChatClass
			lastPage bool
		)

		switch d := resp.(type) {
		case *tg.MessagesDialogs:
			dialogs, messages, users, chats = d.Dialogs, d.Messages, d.Users, d.Chats
			lastPage = true
		case *tg.MessagesDialogsSlice:
			dialogs, messages, users, chats = d.Dialogs, d.Messages, d.Users, d.Chats
			lastPage = len(d.Dialogs) == 0
		default: // *tg.MessagesDialogsNotModified or unexpected
			lastPage = true
		}

		em := newEntityMaps(users, chats)

		// Index each peer's top message so we can read the offset date/id from
		// the last dialog of the page.
		msgByPeer := make(map[peerLookupKey]tg.NotEmptyMessage, len(messages))
		for _, mc := range messages {
			if m, ok := mc.AsNotEmpty(); ok {
				msgByPeer[peerKeyOf(m.GetPeerID())] = m
			}
		}

		for _, dc := range dialogs {
			dialog, ok := dc.(*tg.Dialog)
			if !ok {
				continue
			}

			chatCount++
			if chatCount%100 == 0 && onProgress != nil {
				onProgress(chatCount, fmt.Sprintf("Processed %d chats...", chatCount))
			}

			info, ok := em.dialogToChatInfo(dialog, startTime)
			if !ok {
				continue
			}
			chatsList = append(chatsList, info)
		}

		if lastPage || len(dialogs) == 0 {
			break
		}

		// Advance the offset to the last dialog of the page.
		lastPeer := dialogs[len(dialogs)-1].GetPeer()
		if m, ok := msgByPeer[peerKeyOf(lastPeer)]; ok {
			offsetID = m.GetID()
			offsetDate = m.GetDate()
		}
		offset = offsetPeer(lastPeer, em)
	}

	if onProgress != nil {
		onProgress(chatCount, fmt.Sprintf("Finished: %d chats fetched in %v", chatCount, time.Since(startTime)))
	}

	return &ChatsList{
		Chats: chatsList,
		Count: len(chatsList),
	}, nil
}

// GetPinnedChats retrieves only pinned chats using Telegram's dedicated
// messages.getPinnedDialogs API. This is one API call regardless of how many
// total dialogs the user has, vs. paginating through ALL dialogs and filtering
// client-side.
func GetPinnedChats(ctx context.Context, client *tg.Client) ([]ChatInfo, error) {
	// folderid=0 is the default folder ("All chats"). Pinned dialogs in
	// archived folders are uncommon enough to ignore for resource listing.
	result, err := client.MessagesGetPinnedDialogs(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("getting pinned dialogs: %w", err)
	}

	em := newEntityMaps(result.Users, result.Chats)

	startTime := time.Now()
	pinned := make([]ChatInfo, 0, len(result.Dialogs))
	for _, d := range result.Dialogs {
		dialog, ok := d.(*tg.Dialog)
		if !ok || !dialog.Pinned {
			continue
		}
		info, ok := em.dialogToChatInfo(dialog, startTime)
		if !ok {
			continue
		}
		pinned = append(pinned, info)
	}

	return pinned, nil
}
