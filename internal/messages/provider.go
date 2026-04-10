package messages

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/gotd/td/tg"
	"go.uber.org/ratelimit"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// Provider fetches messages from Telegram with a unified interface.
type Provider struct {
	client  *tg.Client
	limiter ratelimit.Limiter
}

// DefaultRateLimitRPS is the default per-provider request rate (requests per
// second) for history-fetching calls. Kept conservative to avoid tripping
// Telegram's flood-wait on bursty tools like BackupMessages. Override via
// NewProviderWithRate when you need a different ceiling.
const DefaultRateLimitRPS = 1

// NewProvider creates a new message provider with the default 1 RPS rate limit.
func NewProvider(client *tg.Client) *Provider {
	return NewProviderWithRate(client, DefaultRateLimitRPS)
}

// NewProviderWithRate creates a new message provider with an explicit
// requests-per-second limit. Values ≤ 0 fall back to DefaultRateLimitRPS
// so the limiter can never be constructed with a zero/negative rate (which
// would block forever).
func NewProviderWithRate(client *tg.Client, rps int) *Provider {
	if rps <= 0 {
		rps = DefaultRateLimitRPS
	}
	return &Provider{
		client:  client,
		limiter: ratelimit.New(rps),
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
//
// Date filters:
//   - opts.MaxDate maps to Telegram's native offset_date (returns messages
//     strictly older than the cutoff). Callers wanting inclusive day bounds
//     must adjust before passing (see normalizeInclusiveUpperDate). Ignored
//     when opts.OffsetDate is already set by the caller (e.g. FetchAll's
//     pagination loop).
//   - opts.MinDate has no native equivalent and is applied as a post-filter
//     here. Because history comes back reverse-chronological, the first
//     message older than MinDate means we've walked past the window — we
//     drop it and every earlier message, and clamp HasMore to false so the
//     caller doesn't try to paginate off the end.
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

	if offsetDate := historyOffsetDate(opts); !offsetDate.IsZero() {
		historyRequest.OffsetDate = int(offsetDate.Unix())
	}

	if opts.UnreadOnly && readInboxMaxID > 0 {
		historyRequest.MinID = readInboxMaxID
	}

	p.limiter.Take()

	history, err := p.client.MessagesGetHistory(ctx, historyRequest)
	if err != nil {
		return nil, fmt.Errorf("getting messages: %w", err)
	}

	result, err := p.processHistory(history, peer)
	if err != nil {
		return nil, err
	}

	// Apply MinDate post-filter. History is reverse-chronological, so we walk
	// from the newest forward and stop at the first message older than
	// MinDate — everything from that point on is outside the window.
	if !opts.MinDate.IsZero() && len(result.Messages) > 0 {
		cutoff := len(result.Messages)
		for i, m := range result.Messages {
			if m.Date.Before(opts.MinDate) {
				cutoff = i
				break
			}
		}
		if cutoff < len(result.Messages) {
			result.Messages = result.Messages[:cutoff]
			result.Count = len(result.Messages)
			result.HasMore = false
			result.NextID = 0
			if len(result.Messages) > 0 {
				result.NextID = result.Messages[len(result.Messages)-1].ID
			}
		}
	}

	return result, nil
}

// historyOffsetDate returns the exact timestamp to pass to Telegram's
// offset_date parameter. OffsetDate is an internal pagination cursor seeded
// from MaxDate on the first batch, and takes precedence when non-zero.
// MaxDate is an exclusive upper bound (strictly-less-than) as documented in
// FetchOptions; do not widen it here or RFC3339 timestamps like
// 2026-04-10T15:00:00Z would silently grow by 24h.
func historyOffsetDate(opts FetchOptions) time.Time {
	if !opts.OffsetDate.IsZero() {
		return opts.OffsetDate
	}
	return opts.MaxDate
}

// FetchContext retrieves a window of messages around the given anchor:
// [anchor-before, …, anchor, …, anchor+after], returned in chronological
// order. Uses Telegram's native add_offset parameter so the whole window
// comes back in a single API call.
//
// A negative add_offset shifts the cursor forward in time (towards newer
// messages) relative to offset_id, which is the only way to fetch messages
// newer than a specific ID. When after > 0, the returned slice may be
// shorter than requested if the anchor is near the end of the chat.
func (p *Provider) FetchContext(ctx context.Context, chatID int64, anchorID, before, after int) (*FetchResult, error) {
	peer, err := tgclient.ResolvePeer(ctx, p.client, chatID)
	if err != nil {
		return nil, fmt.Errorf("resolving peer: %w", err)
	}

	// Standard Telethon-style context fetch:
	//   offset_id = anchorID, add_offset = -after, limit = before+after+1
	//
	// Telegram's messages.getHistory returns messages with ID < offset_id
	// starting at (offset_id + add_offset). With add_offset = -after it
	// shifts the window forward by `after` positions, so the returned slice
	// contains at most `after` messages newer than the anchor, the anchor
	// itself, and at most `before` messages older than the anchor — in
	// reverse-chronological order, exactly the window we want.
	limit := before + after + 1
	req := &tg.MessagesGetHistoryRequest{
		Peer:      peer,
		OffsetID:  anchorID,
		AddOffset: -after,
		Limit:     limit,
	}

	p.limiter.Take()
	history, err := p.client.MessagesGetHistory(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("getting message context: %w", err)
	}

	result, err := p.processHistory(history, peer)
	if err != nil {
		return nil, err
	}

	// Flip reverse-chronological → chronological so downstream consumers
	// (and the LLM) see the natural reading order.
	for i, j := 0, len(result.Messages)-1; i < j; i, j = i+1, j-1 {
		result.Messages[i], result.Messages[j] = result.Messages[j], result.Messages[i]
	}
	result.ChatID = chatID
	// HasMore / NextID don't apply to a fixed window — clear them to avoid
	// confusing callers into paginating past the window.
	result.HasMore = false
	result.NextID = 0
	return result, nil
}

// FetchScheduled retrieves all scheduled (pending) messages for a chat.
// Scheduled messages are returned by Telegram as a single unpaginated list
// and live in a separate ID space from the regular history — callers MUST
// treat their IDs as distinct from regular message IDs (see MessageRef and
// the "s:" handle prefix at the tool boundary).
//
// Returns an empty FetchResult (not an error) when the chat has no pending
// scheduled messages, so callers can render "no pending" without branching
// on error vs empty.
func (p *Provider) FetchScheduled(ctx context.Context, chatID int64) (*FetchResult, error) {
	peer, err := tgclient.ResolvePeer(ctx, p.client, chatID)
	if err != nil {
		return nil, fmt.Errorf("resolving peer: %w", err)
	}

	p.limiter.Take()
	history, err := p.client.MessagesGetScheduledHistory(ctx, &tg.MessagesGetScheduledHistoryRequest{
		Peer: peer,
	})
	if err != nil {
		return nil, fmt.Errorf("getting scheduled messages: %w", err)
	}

	result, err := p.processHistory(history, peer)
	if err != nil {
		return nil, err
	}
	result.ChatID = chatID
	// Scheduled messages don't paginate — clear fields that only make sense
	// for regular history to avoid confusing callers.
	result.HasMore = false
	result.NextID = 0
	return result, nil
}

// FetchAll retrieves all messages matching the options, handling pagination
// automatically. The onBatch callback is called after each batch is fetched
// (can be nil).
//
// Partial-result contract: when the context is cancelled mid-fetch the
// accumulated messages collected so far are returned alongside a non-nil
// error equal to ctx.Err() (context.Canceled or context.DeadlineExceeded).
// Callers that want to persist work on interruption (BackupMessages) can check
// errors.Is(err, context.Canceled) or errors.Is(err, context.DeadlineExceeded)
// and still use the partial FetchResult — any other error is a hard failure.
func (p *Provider) FetchAll(ctx context.Context, chatID int64, opts FetchOptions, onBatch BatchCallback) (*FetchResult, error) {
	peer, err := tgclient.ResolvePeer(ctx, p.client, chatID)
	if err != nil {
		return nil, fmt.Errorf("resolving peer: %w", err)
	}

	result, err := p.fetchAllWithPeer(ctx, peer, opts, onBatch)
	if result != nil {
		result.ChatID = chatID
	}
	return result, err
}

// fetchAllWithPeer retrieves all messages using an already resolved peer.
func (p *Provider) fetchAllWithPeer(ctx context.Context, peer tg.InputPeerClass, opts FetchOptions, onBatch BatchCallback) (*FetchResult, error) {
	// Pre-allocate the messages slice when MaxCount bounds the result size
	// so the append loop below doesn't re-grow the underlying array.
	initialCap := max(opts.MaxCount, 0)
	result := &FetchResult{
		Messages: make([]Message, 0, initialCap),
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
		batchOpts.OffsetDate = opts.MaxDate
	}

	batchNum := 0

	for {
		select {
		case <-ctx.Done():
			result.Count = len(result.Messages)
			return result, ctx.Err()
		default:
		}

		batchNum++

		batch, err := p.fetchWithPeer(ctx, peer, batchOpts)
		if err != nil {
			// Return whatever we've already collected alongside the error
			// so callers (e.g. BackupMessages) can persist partial progress
			// instead of losing everything to a mid-pagination failure.
			result.Count = len(result.Messages)
			return result, fmt.Errorf("fetching batch %d: %w", batchNum, err)
		}

		// Merge user/chat maps
		maps.Copy(result.Users, batch.Users)
		maps.Copy(result.Chats, batch.Chats)

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
	case *tg.MessagesMessagesNotModified:
		// Telegram signals "nothing changed" — return an empty page so callers
		// behave as if the history is exhausted rather than erroring out.
		return result, nil
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
		if msg.ID <= 0 {
			// Telegram guarantees positive IDs; a zero-ID message would cause
			// FormatRegularRef/FormatScheduledRef to panic at the tool boundary.
			slog.Warn("extractMessages: dropping message with non-positive ID; Telegram API contract violation", "msg_id", msg.ID)
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
		slog.Warn("getReadInboxMaxID: unexpected dialog type; unread filter will be inactive",
			"type", fmt.Sprintf("%T", result.Dialogs[0]))
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
		slog.Warn("extractSender: unrecognized peer type", "type", fmt.Sprintf("%T", peer))
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
				// Get the largest photo size for dimensions and thumb type
				var thumbType string
				for _, size := range p.Sizes {
					var w, h int
					var sizeType string
					switch s := size.(type) {
					case *tg.PhotoSize:
						w, h, sizeType = s.W, s.H, s.Type
					case *tg.PhotoSizeProgressive:
						w, h, sizeType = s.W, s.H, s.Type
					case *tg.PhotoCachedSize:
						w, h, sizeType = s.W, s.H, s.Type
					default:
						continue
					}
					if w > info.Width {
						info.Width = w
						info.Height = h
						thumbType = sizeType
					}
				}
				// Generate resource URI for photo download
				if thumbType != "" {
					fileRef := base64.URLEncoding.EncodeToString(p.FileReference)
					info.ResourceURI = fmt.Sprintf(
						"telegram://media/%d/%d/%d/%s?ref=%s",
						p.ID, p.AccessHash, p.DCID, thumbType, fileRef,
					)
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
