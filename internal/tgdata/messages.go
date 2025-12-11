package tgdata

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessagesOptions configures the GetMessages request
type MessagesOptions struct {
	Limit      int
	OffsetID   int
	UnreadOnly bool
}

// DefaultMessagesOptions returns default options for GetMessages
func DefaultMessagesOptions() MessagesOptions {
	return MessagesOptions{
		Limit:      50,
		OffsetID:   0,
		UnreadOnly: false,
	}
}

// GetMessages retrieves messages from a specific chat
func GetMessages(ctx context.Context, client *tg.Client, chatID int64, opts MessagesOptions) (*MessagesList, error) {
	peer, err := tgclient.ResolvePeer(ctx, client, chatID)
	if err != nil {
		return nil, fmt.Errorf("resolving peer: %w", err)
	}

	var readInboxMaxID int
	if opts.UnreadOnly {
		readInboxMaxID, err = getReadInboxMaxID(ctx, client, peer)
		if err != nil {
			return nil, fmt.Errorf("getting read inbox max id: %w", err)
		}
	}

	historyRequest := &tg.MessagesGetHistoryRequest{
		Peer:     peer,
		Limit:    opts.Limit,
		OffsetID: opts.OffsetID,
	}

	if opts.UnreadOnly && readInboxMaxID > 0 {
		historyRequest.MinID = readInboxMaxID
	}

	history, err := client.MessagesGetHistory(ctx, historyRequest)
	if err != nil {
		return nil, fmt.Errorf("getting messages: %w", err)
	}

	var messages []MessageInfo
	var totalCount int

	switch hist := history.(type) {
	case *tg.MessagesMessages:
		messages = extractMessages(hist.Messages)
		totalCount = len(hist.Messages)
	case *tg.MessagesMessagesSlice:
		messages = extractMessages(hist.Messages)
		totalCount = hist.Count
	case *tg.MessagesChannelMessages:
		messages = extractMessages(hist.Messages)
		totalCount = hist.Count
	}

	result := MessagesList{
		ChatID:   chatID,
		Messages: messages,
		Count:    len(messages),
		HasMore:  len(messages) > 0 && len(messages) < totalCount,
	}

	if len(messages) > 0 {
		result.NextID = messages[len(messages)-1].ID
	}

	return &result, nil
}

func getReadInboxMaxID(ctx context.Context, client *tg.Client, peer tg.InputPeerClass) (int, error) {
	result, err := client.MessagesGetPeerDialogs(ctx, []tg.InputDialogPeerClass{
		&tg.InputDialogPeer{Peer: peer},
	})
	if err != nil {
		return 0, fmt.Errorf("getting peer dialogs: %w", err)
	}

	if len(result.Dialogs) == 0 {
		return 0, nil
	}

	dialog, ok := result.Dialogs[0].(*tg.Dialog)
	if !ok {
		return 0, nil
	}

	return dialog.ReadInboxMaxID, nil
}

func extractMessages(messages []tg.MessageClass) []MessageInfo {
	result := make([]MessageInfo, 0, len(messages))

	for _, msgClass := range messages {
		msg, ok := msgClass.(*tg.Message)
		if !ok {
			continue
		}

		info := MessageInfo{
			ID:   msg.ID,
			Date: time.Unix(int64(msg.Date), 0),
			Text: msg.Message,
		}

		if msg.FromID != nil {
			switch from := msg.FromID.(type) {
			case *tg.PeerUser:
				info.SenderID = from.UserID
			case *tg.PeerChannel:
				info.SenderID = from.ChannelID
			case *tg.PeerChat:
				info.SenderID = from.ChatID
			}
		}

		if msg.ReplyTo != nil {
			if reply, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok {
				info.ReplyToID = reply.ReplyToMsgID
			}
		}

		if msg.Media != nil {
			info.Media = extractMediaType(msg.Media)
		}

		for _, entity := range msg.Entities {
			if url, ok := entity.(*tg.MessageEntityURL); ok {
				if url.Offset >= 0 && url.Offset+url.Length <= len(msg.Message) {
					info.Entities = append(info.Entities, msg.Message[url.Offset:url.Offset+url.Length])
				}
			}
			if textURL, ok := entity.(*tg.MessageEntityTextURL); ok {
				info.Entities = append(info.Entities, textURL.URL)
			}
		}

		result = append(result, info)
	}

	return result
}

func extractMediaType(media tg.MessageMediaClass) *MediaInfo {
	switch media.(type) {
	case *tg.MessageMediaPhoto:
		return &MediaInfo{Type: "photo"}
	case *tg.MessageMediaDocument:
		return &MediaInfo{Type: "document"}
	case *tg.MessageMediaGeo:
		return &MediaInfo{Type: "geo"}
	case *tg.MessageMediaContact:
		return &MediaInfo{Type: "contact"}
	case *tg.MessageMediaWebPage:
		return &MediaInfo{Type: "webpage"}
	case *tg.MessageMediaVenue:
		return &MediaInfo{Type: "venue"}
	case *tg.MessageMediaPoll:
		return &MediaInfo{Type: "poll"}
	case *tg.MessageMediaDice:
		return &MediaInfo{Type: "dice"}
	default:
		return &MediaInfo{Type: "other"}
	}
}
