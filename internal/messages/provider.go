package messages

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
	"go.uber.org/ratelimit"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// offsetDateBuffer is added to MaxDate when fetching messages because
// Telegram's API returns messages BEFORE the offset date, and we want
// to include messages from the MaxDate day itself.
const offsetDateBuffer = 24 * time.Hour

// Provider fetches messages from Telegram with a unified interface.
type Provider struct {
	client  *tg.Client
	limiter ratelimit.Limiter
}

// NewProvider creates a new message provider with 1 RPS rate limiting.
func NewProvider(client *tg.Client) *Provider {
	return &Provider{
		client:  client,
		limiter: ratelimit.New(1),
	}
}

// Fetch retrieves messages from a chat with the given options.
// It handles pagination internally and returns enriched messages with sender names.
func (p *Provider) Fetch(ctx context.Context, chatID int64, opts FetchOptions) (*FetchResult, error) {
	peer, err := tgclient.ResolvePeer(ctx, p.client, chatID)
	if err != nil {
		return nil, fmt.Errorf("resolving peer: %w", err)
	}

	result, err := p.fetchWithPeer(ctx, peer, opts)
	if err != nil {
		return nil, err
	}
	result.ChatID = chatID
	return result, nil
}

// fetchWithPeer retrieves messages using an already resolved peer.
func (p *Provider) fetchWithPeer(ctx context.Context, peer tg.InputPeerClass, opts FetchOptions) (*FetchResult, error) {
	if opts.Limit <= 0 {
		opts.Limit = 50
	}

	var readInboxMaxID int
	var err error
	if opts.UnreadOnly {
		readInboxMaxID, err = p.getReadInboxMaxID(ctx, peer)
		if err != nil {
			return nil, fmt.Errorf("getting read inbox max id: %w", err)
		}
	}

	historyRequest := &tg.MessagesGetHistoryRequest{
		Peer:     peer,
		Limit:    opts.Limit,
		OffsetID: opts.OffsetID,
	}

	if !opts.OffsetDate.IsZero() {
		historyRequest.OffsetDate = int(opts.OffsetDate.Unix())
	}

	if opts.UnreadOnly && readInboxMaxID > 0 {
		historyRequest.MinID = readInboxMaxID
	}

	p.limiter.Take()

	history, err := p.client.MessagesGetHistory(ctx, historyRequest)
	if err != nil {
		return nil, fmt.Errorf("getting messages: %w", err)
	}

	return p.processHistory(history, peer)
}

// FetchAll retrieves all messages matching the options, handling pagination automatically.
// The onBatch callback is called after each batch is fetched (can be nil).
func (p *Provider) FetchAll(ctx context.Context, chatID int64, opts FetchOptions, onBatch BatchCallback) (*FetchResult, error) {
	peer, err := tgclient.ResolvePeer(ctx, p.client, chatID)
	if err != nil {
		return nil, fmt.Errorf("resolving peer: %w", err)
	}

	result, err := p.fetchAllWithPeer(ctx, peer, opts, onBatch)
	if err != nil {
		return nil, err
	}
	result.ChatID = chatID
	return result, nil
}

// fetchAllWithPeer retrieves all messages using an already resolved peer.
func (p *Provider) fetchAllWithPeer(ctx context.Context, peer tg.InputPeerClass, opts FetchOptions, onBatch BatchCallback) (*FetchResult, error) {
	result := &FetchResult{
		Messages: make([]Message, 0),
		Users:    make(map[int64]string),
		Chats:    make(map[int64]string),
	}

	batchOpts := FetchOptions{
		Limit: opts.Limit,
	}
	if batchOpts.Limit <= 0 {
		batchOpts.Limit = 100
	}

	// Set the initial offset date if MaxDate is specified.
	if !opts.MaxDate.IsZero() {
		batchOpts.OffsetDate = opts.MaxDate.Add(offsetDateBuffer)
	}

	batchNum := 0

	for {
		select {
		case <-ctx.Done():
			result.Count = len(result.Messages)
			result.Total = len(result.Messages)
			return result, fmt.Errorf("context canceled: %w", ctx.Err())
		default:
		}

		batchNum++

		batch, err := p.fetchWithPeer(ctx, peer, batchOpts)
		if err != nil {
			return nil, fmt.Errorf("fetching batch %d: %w", batchNum, err)
		}

		// Merge user/chat maps
		for k, v := range batch.Users {
			result.Users[k] = v
		}
		for k, v := range batch.Chats {
			result.Chats[k] = v
		}

		if len(batch.Messages) == 0 {
			if onBatch != nil {
				onBatch(batchNum, len(result.Messages), time.Time{})
			}
			break
		}

		// Find the earliest message time in this batch for progress tracking
		var earliestTime time.Time
		for _, msg := range batch.Messages {
			if earliestTime.IsZero() || msg.Date.Before(earliestTime) {
				earliestTime = msg.Date
			}
		}

		// Filter and collect messages
		reachedMinDate := false
		for _, msg := range batch.Messages {
			// Check min date filter
			if !opts.MinDate.IsZero() && msg.Date.Before(opts.MinDate) {
				reachedMinDate = true
				break
			}

			result.Messages = append(result.Messages, msg)

			// Check max count
			if opts.MaxCount > 0 && len(result.Messages) >= opts.MaxCount {
				result.Count = len(result.Messages)
				result.Total = len(result.Messages)
				if onBatch != nil {
					onBatch(batchNum, len(result.Messages), earliestTime)
				}
				return result, nil
			}
		}

		// Call batch callback with progress info
		if onBatch != nil {
			onBatch(batchNum, len(result.Messages), earliestTime)
		}

		// Stop conditions
		if reachedMinDate || !batch.HasMore {
			break
		}

		// Update offset for the next batch
		batchOpts.OffsetID = batch.NextID
		batchOpts.OffsetDate = time.Time{} // Reset after the first batch
	}

	result.Count = len(result.Messages)
	result.Total = len(result.Messages)
	return result, nil
}

func (p *Provider) processHistory(history tg.MessagesMessagesClass, peer tg.InputPeerClass) (*FetchResult, error) {
	result := &FetchResult{
		Users: make(map[int64]string),
		Chats: make(map[int64]string),
	}

	var messages []tg.MessageClass
	var users []tg.UserClass
	var chats []tg.ChatClass
	var totalCount int

	switch hist := history.(type) {
	case *tg.MessagesMessages:
		messages = hist.Messages
		users = hist.Users
		chats = hist.Chats
		totalCount = len(hist.Messages)
	case *tg.MessagesMessagesSlice:
		messages = hist.Messages
		users = hist.Users
		chats = hist.Chats
		totalCount = hist.Count
	case *tg.MessagesChannelMessages:
		messages = hist.Messages
		users = hist.Users
		chats = hist.Chats
		totalCount = hist.Count
	default:
		return nil, fmt.Errorf("unexpected response type: %T", history)
	}

	// Build user/chat maps
	for _, u := range users {
		if user, ok := u.(*tg.User); ok {
			result.Users[user.ID] = tgclient.UserName(user)
		}
	}
	for _, c := range chats {
		switch chat := c.(type) {
		case *tg.Chat:
			result.Chats[chat.ID] = chat.Title
		case *tg.Channel:
			result.Chats[chat.ID] = chat.Title
		}
	}

	// Extract messages
	result.Messages = p.extractMessages(messages, result.Users, result.Chats, peer)
	result.Count = len(result.Messages)
	result.Total = totalCount
	result.HasMore = len(result.Messages) > 0 && len(result.Messages) < totalCount

	if len(result.Messages) > 0 {
		result.NextID = result.Messages[len(result.Messages)-1].ID
	}

	return result, nil
}

func (p *Provider) extractMessages(messages []tg.MessageClass, users map[int64]string, chats map[int64]string, peer tg.InputPeerClass) []Message {
	result := make([]Message, 0, len(messages))

	for _, msgClass := range messages {
		msg, ok := msgClass.(*tg.Message)
		if !ok {
			continue
		}

		m := Message{
			ID:   msg.ID,
			Date: time.Unix(int64(msg.Date), 0),
			Text: msg.Message,
			Raw:  msg,
		}

		// Extract sender
		if msg.FromID != nil {
			m.SenderID, m.SenderName = extractSender(msg.FromID, users, chats)
		} else {
			// In private chats, messages from the other user have FromID == nil
			// Use the peer (chat partner) as the sender
			m.SenderID, m.SenderName = extractSender(peer, users, chats)
		}

		// Extract reply info
		if msg.ReplyTo != nil {
			if reply, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok {
				m.ReplyToID = reply.ReplyToMsgID
			}
		}

		// Extract media type
		if msg.Media != nil {
			m.Media = extractMediaType(msg.Media)
		}

		// Extract entities (URLs)
		// Note: Telegram uses UTF-16 code units for offset/length
		for _, entity := range msg.Entities {
			if url, ok := entity.(*tg.MessageEntityURL); ok {
				if extracted := extractSubstring(msg.Message, url.Offset, url.Length); extracted != "" {
					m.Entities = append(m.Entities, extracted)
				}
			}
			if textURL, ok := entity.(*tg.MessageEntityTextURL); ok {
				m.Entities = append(m.Entities, textURL.URL)
			}
		}

		result = append(result, m)
	}

	return result
}

func (p *Provider) getReadInboxMaxID(ctx context.Context, peer tg.InputPeerClass) (int, error) {
	result, err := p.client.MessagesGetPeerDialogs(ctx, []tg.InputDialogPeerClass{
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

// extractSender extracts sender ID and name from a PeerClass or InputPeerClass.
func extractSender(peer any, users map[int64]string, chats map[int64]string) (int64, string) {
	var id int64
	var name string

	const unknownSender = "Unknown"

	switch p := peer.(type) {
	case interface{ GetUserID() int64 }:
		id = p.GetUserID()
		name = users[id]
	case interface{ GetChannelID() int64 }:
		id = p.GetChannelID()
		name = chats[id]
	case interface{ GetChatID() int64 }:
		id = p.GetChatID()
		name = chats[id]
	default:
		return 0, unknownSender
	}

	if name == "" {
		name = unknownSender
	}
	return id, name
}

func extractMediaType(media tg.MessageMediaClass) *MediaInfo {
	switch m := media.(type) {
	case *tg.MessageMediaPhoto:
		info := &MediaInfo{Type: "photo"}
		if photo, ok := m.GetPhoto(); ok {
			if p, ok := photo.(*tg.Photo); ok {
				// Get the largest photo size for dimensions
				for _, size := range p.Sizes {
					var w, h int
					switch s := size.(type) {
					case *tg.PhotoSize:
						w, h = s.W, s.H
					case *tg.PhotoSizeProgressive:
						w, h = s.W, s.H
					case *tg.PhotoCachedSize:
						w, h = s.W, s.H
					default:
						continue
					}
					if w > info.Width {
						info.Width = w
						info.Height = h
					}
				}
			}
		}
		return info
	case *tg.MessageMediaDocument:
		info := &MediaInfo{Type: "document"}
		if doc, ok := m.GetDocument(); ok {
			if d, ok := doc.(*tg.Document); ok {
				for _, attr := range d.Attributes {
					if fileName, ok := attr.(*tg.DocumentAttributeFilename); ok {
						info.FileName = fileName.FileName
						break
					}
				}
			}
		}
		return info
	case *tg.MessageMediaGeo:
		return &MediaInfo{Type: "geo"}
	case *tg.MessageMediaContact:
		return &MediaInfo{Type: "contact"}
	case *tg.MessageMediaWebPage:
		info := &MediaInfo{Type: "webpage"}
		if webpage, ok := m.Webpage.(*tg.WebPage); ok {
			info.URL = webpage.URL
		}
		return info
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

// extractSubstring extracts a substring using UTF-16 code unit offsets.
// Telegram uses UTF-16 for entity positions: emoji = 2 units, other chars = 1 unit.
func extractSubstring(s string, offset, length int) string {
	if offset < 0 || length <= 0 {
		return ""
	}

	runes := []rune(s)
	end := offset + length

	// Convert UTF-16 offset to rune index
	pos := 0
	start := -1
	stop := -1

	for i, r := range runes {
		if pos >= offset && start < 0 {
			start = i
		}
		if r > 0xFFFF {
			pos += 2
		} else {
			pos++
		}
		if pos >= end {
			stop = i + 1
			break
		}
	}

	if start < 0 || stop < 0 {
		return ""
	}

	return string(runes[start:stop])
}
