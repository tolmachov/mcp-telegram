package messages

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gotd/td/tg"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// Search runs a substring search inside a single chat via messages.search.
// Pagination mirrors Fetch: pass the previous result's NextID as OffsetID.
// Date filters map to Telegram's native MinDate/MaxDate — no post-filter is
// needed because messages.search honours them server-side.
func (p *Provider) Search(ctx context.Context, chatID int64, opts SearchOptions) (*FetchResult, error) {
	if opts.Query == "" {
		return nil, fmt.Errorf("search query is required")
	}
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	if !opts.MinDate.IsZero() && !opts.MaxDate.IsZero() && opts.MinDate.After(opts.MaxDate) {
		return nil, fmt.Errorf("min_date (%s) is after max_date (%s): the date window is empty",
			opts.MinDate.Format(time.RFC3339), opts.MaxDate.Format(time.RFC3339))
	}

	peer, err := tgclient.ResolvePeer(ctx, p.client, chatID)
	if err != nil {
		return nil, fmt.Errorf("resolving peer: %w", err)
	}

	filter := opts.Filter
	if filter == nil {
		filter = &tg.InputMessagesFilterEmpty{}
	}

	req := &tg.MessagesSearchRequest{
		Peer:     peer,
		Q:        opts.Query,
		Filter:   filter,
		OffsetID: opts.OffsetID,
		Limit:    opts.Limit,
	}
	if !opts.MinDate.IsZero() {
		req.MinDate = int(opts.MinDate.Unix())
	}
	if !opts.MaxDate.IsZero() {
		req.MaxDate = int(opts.MaxDate.Unix())
	}
	if opts.TopMsgID > 0 {
		req.SetTopMsgID(opts.TopMsgID)
	}
	if opts.FromSenderID != 0 {
		fromPeer, err := tgclient.ResolvePeer(ctx, p.client, opts.FromSenderID)
		if err != nil {
			return nil, fmt.Errorf("resolving from_sender %d: %w", opts.FromSenderID, err)
		}
		// messages.search from_id only accepts user or channel peers.
		// Basic-chat peers (legacy InputPeerChat) are silently dropped by
		// Telegram, turning what the caller framed as a sender filter into
		// an un-filtered search. Reject loudly instead.
		if _, isBasicChat := fromPeer.(*tg.InputPeerChat); isBasicChat {
			return nil, fmt.Errorf("from_sender_id %d resolves to a legacy basic chat, which cannot be used as a message sender; pass a user ID or a channel/supergroup ID", opts.FromSenderID)
		}
		req.SetFromID(fromPeer)
	}

	p.limiter.Take()

	history, err := p.client.MessagesSearch(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("searching messages: %w", err)
	}

	result, err := p.processHistory(history, peer)
	if err != nil {
		return nil, err
	}
	result.ChatID = chatID
	return result, nil
}

// SearchGlobal runs a substring search across all chats the user is in,
// via messages.searchGlobal. Pagination uses an opaque cursor built from
// the (offset_rate, offset_peer, offset_id) tuple of the prior response.
// Only standard FLOOD_WAIT applies — the per-day `searchPostsFlood` quota
// documented for hashtag search (channels.searchPosts) does not affect
// this method.
func (p *Provider) SearchGlobal(ctx context.Context, opts GlobalSearchOptions) (*GlobalSearchResult, error) {
	if opts.Query == "" {
		return nil, fmt.Errorf("search query is required")
	}
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	if !opts.MinDate.IsZero() && !opts.MaxDate.IsZero() && opts.MinDate.After(opts.MaxDate) {
		return nil, fmt.Errorf("min_date (%s) is after max_date (%s): the date window is empty",
			opts.MinDate.Format(time.RFC3339), opts.MaxDate.Format(time.RFC3339))
	}

	req := &tg.MessagesSearchGlobalRequest{
		Q:      opts.Query,
		Filter: &tg.InputMessagesFilterEmpty{},
		Limit:  opts.Limit,
	}
	if opts.Cursor != nil {
		req.OffsetPeer = opts.Cursor.InputPeer()
		req.OffsetRate = opts.Cursor.Rate
		req.OffsetID = opts.Cursor.MsgID
	} else {
		req.OffsetPeer = &tg.InputPeerEmpty{}
	}
	if !opts.MinDate.IsZero() {
		req.MinDate = int(opts.MinDate.Unix())
	}
	if !opts.MaxDate.IsZero() {
		req.MaxDate = int(opts.MaxDate.Unix())
	}

	p.limiter.Take()

	history, err := p.client.MessagesSearchGlobal(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("global searching messages: %w", err)
	}

	return p.processGlobalHistory(history, opts.Limit)
}

// processGlobalHistory converts a cross-chat search response into per-chat
// attributed messages. Unlike processHistory (which knows the single owning
// peer up front), this walks msg.PeerID for each message to determine the
// chat it belongs to and look up the display title. It also extracts the
// next pagination cursor from the last message when the response is a slice
// and exposes next_rate.
func (p *Provider) processGlobalHistory(history tg.MessagesMessagesClass, limit int) (*GlobalSearchResult, error) {
	var rawMessages []tg.MessageClass
	var users []tg.UserClass
	var chats []tg.ChatClass
	var nextRate int
	var hasNextRate bool

	switch hist := history.(type) {
	case *tg.MessagesMessages:
		rawMessages = hist.Messages
		users = hist.Users
		chats = hist.Chats
	case *tg.MessagesMessagesSlice:
		rawMessages = hist.Messages
		users = hist.Users
		chats = hist.Chats
		if rate, ok := hist.GetNextRate(); ok {
			nextRate = rate
			hasNextRate = true
		}
	case *tg.MessagesChannelMessages:
		rawMessages = hist.Messages
		users = hist.Users
		chats = hist.Chats
	case *tg.MessagesMessagesNotModified:
		// Telegram signals "nothing changed" — return an empty result so the
		// caller sees zero matches rather than a hard error.
		return &GlobalSearchResult{Messages: make([]GlobalMessage, 0)}, nil
	default:
		return nil, fmt.Errorf("unexpected response type: %T", history)
	}

	// Build per-ID maps once so extractGlobalChat is O(1) per message instead
	// of O(users+chats). For a typical page of 50 messages spanning dozens of
	// dialogs this saves a noticeable amount of work; more importantly it
	// scales cleanly if limit grows.
	userMap := make(map[int64]string, len(users))
	userByID := make(map[int64]*tg.User, len(users))
	for _, u := range users {
		if user, ok := u.(*tg.User); ok {
			userMap[user.ID] = tgclient.UserName(user)
			userByID[user.ID] = user
		}
	}

	// Three parallel maps for chat lookups:
	//   - chatTitleMap holds the *display title* keyed by the raw Telegram ID.
	//   - chatIDByRaw maps a raw Telegram ID to the bare MTProto ID we return to
	//     the caller. Bare IDs equal the raw ID for every peer type, so this is
	//     now effectively an identity map; it is kept as a single seam in case
	//     the returned ID format ever needs to diverge again.
	//   - channelByID holds full channel records so access_hash lookups for
	//     cursor construction are O(1).
	chatTitleMap := make(map[int64]string, len(chats))
	chatIDByRaw := make(map[int64]int64, len(chats))
	channelByID := make(map[int64]*tg.Channel, len(chats))
	for _, c := range chats {
		switch chat := c.(type) {
		case *tg.Chat:
			chatTitleMap[chat.ID] = chat.Title
			chatIDByRaw[chat.ID] = chat.ID
		case *tg.Channel:
			chatTitleMap[chat.ID] = chat.Title
			chatIDByRaw[chat.ID] = chat.ID
			channelByID[chat.ID] = chat
		}
	}

	result := &GlobalSearchResult{
		Messages: make([]GlobalMessage, 0, len(rawMessages)),
	}

	skipped := 0

	for _, msgClass := range rawMessages {
		msg, ok := msgClass.(*tg.Message)
		if !ok {
			// MessageService, MessageEmpty, or any future message class the
			// search path cannot render. Count and skip.
			skipped++
			continue
		}

		// Derive the chat this message lives in from msg.PeerID.
		// For DMs the sender (FromID) is typically nil and the "other side"
		// is the peer itself, so we also reuse peerID as the fallback for
		// extractSender below. An unknown PeerClass yields an empty
		// peerKind — skip the message rather than emitting zero
		// attribution to the LLM, which would show up as a chat_id=0
		// result the caller cannot do anything with.
		chatID, chatTitle, peerKind, _, _ := extractGlobalChat(msg.PeerID, userMap, chatTitleMap, chatIDByRaw, userByID, channelByID)
		if peerKind == "" {
			slog.Debug("SearchMessagesGlobal: skipping message with unknown peer kind", "msg_id", msg.ID, "peer_type", fmt.Sprintf("%T", msg.PeerID))
			skipped++
			continue
		}

		m := Message{
			ID:   msg.ID,
			Date: time.Unix(int64(msg.Date), 0),
			Text: msg.Message,
			Raw:  msg,
		}

		if msg.FromID != nil {
			m.SenderID, m.SenderName = extractSender(msg.FromID, userMap, chatTitleMap)
		} else {
			// DM fallback: sender is the other end of the conversation.
			m.SenderID, m.SenderName = extractSender(msg.PeerID, userMap, chatTitleMap)
		}

		if msg.ReplyTo != nil {
			m.ReplyToID = extractReplyToID(msg.ReplyTo)
		}
		if msg.Media != nil {
			m.Media = extractMediaType(msg.Media)
		}
		if replies, ok := msg.GetReplies(); ok {
			m.Replies = extractReplies(replies)
		}
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

		result.Messages = append(result.Messages, GlobalMessage{
			Message:   m,
			ChatID:    chatID,
			ChatTitle: chatTitle,
		})
	}

	result.Count = len(result.Messages)
	result.SkippedCount = skipped

	// Build the next cursor when the raw page came back full AND we have a
	// last-message anchor. Gating on the *raw* page length (not the filtered
	// result) matters: Telegram may return MessageService / MessageEmpty
	// slots that we drop above, so filtering could otherwise make a full
	// page look partial and silently end pagination mid-stream. next_rate is
	// optional and defaults to 0 when absent.
	if len(rawMessages) >= limit {
		lastPeerKind, lastPeerID, lastAccessHash, lastMsgID := lastGlobalCursorAnchor(rawMessages, userByID, channelByID)
		if lastPeerKind == "" || lastMsgID <= 0 {
			return result, nil
		}
		rate := 0
		if hasNextRate {
			rate = nextRate
		}
		cursor, err := NewGlobalSearchCursor(rate, lastPeerKind, lastPeerID, lastAccessHash, lastMsgID)
		if err != nil {
			// A failing invariant here means processGlobalHistory built an
			// inconsistent cursor — e.g. a user/channel peer with no
			// access_hash. Surface it instead of silently truncating
			// pagination; tests should catch this.
			return nil, fmt.Errorf("building next cursor: %w", err)
		}
		result.NextCursor = cursor
		result.HasMore = true
	}

	return result, nil
}

// extractGlobalChat returns everything needed to both report a message's
// chat to the caller and to rebuild an InputPeer for pagination:
//
//	chatID         — bare MTProto id (positive for every peer type)
//	chatTitle      — display name for UI
//	peerKind       — "user" | "chat" | "channel" (drives cursor round-trip)
//	peerRawID      — same bare id, named separately because it feeds InputPeer
//	                 reconstruction for pagination (chatID == peerRawID today)
//	accessHash     — required for user/channel InputPeer; 0 for basic chat
//
// The userByID / channelByID maps are pre-built by the caller so access_hash
// lookups are O(1) instead of a linear scan per message.
func extractGlobalChat(
	peer tg.PeerClass,
	users map[int64]string,
	chatTitles map[int64]string,
	chatIDByRaw map[int64]int64,
	userByID map[int64]*tg.User,
	channelByID map[int64]*tg.Channel,
) (chatID int64, chatTitle string, peerKind PeerKind, peerRawID int64, accessHash int64) {
	switch p := peer.(type) {
	case *tg.PeerUser:
		peerKind = PeerKindUser
		peerRawID = p.UserID
		chatID = p.UserID
		chatTitle = users[p.UserID]
		if user, ok := userByID[p.UserID]; ok {
			accessHash = user.AccessHash
		}
	case *tg.PeerChat:
		peerKind = PeerKindChat
		peerRawID = p.ChatID
		chatID = chatIDByRaw[p.ChatID]
		if chatID == 0 {
			chatID = p.ChatID
		}
		chatTitle = chatTitles[p.ChatID]
	case *tg.PeerChannel:
		peerKind = PeerKindChannel
		peerRawID = p.ChannelID
		chatID = chatIDByRaw[p.ChannelID]
		if chatID == 0 {
			chatID = p.ChannelID
		}
		chatTitle = chatTitles[p.ChannelID]
		if ch, ok := channelByID[p.ChannelID]; ok {
			accessHash = ch.AccessHash
		}
	}
	return
}

// lastGlobalCursorAnchor walks the raw page backwards and extracts the last
// message tuple that can safely seed the next searchGlobal cursor. This must
// operate on the raw page rather than the filtered result set: service
// messages and other skipped items still advance Telegram pagination, so using
// the last rendered message can duplicate tail items on the next page.
func lastGlobalCursorAnchor(
	rawMessages []tg.MessageClass,
	userByID map[int64]*tg.User,
	channelByID map[int64]*tg.Channel,
) (peerKind PeerKind, peerRawID, accessHash int64, msgID int) {
	for i := len(rawMessages) - 1; i >= 0; i-- {
		switch msg := rawMessages[i].(type) {
		case *tg.Message:
			if msg.ID <= 0 || msg.PeerID == nil {
				continue
			}
			// nil maps for users/chatTitles/chatIDByRaw are safe: only
			// peerKind, peerRawID, and accessHash are used for cursor
			// construction. chatID and chatTitle may be partially populated
			// (users/channels still compute them from the peer directly) but
			// the caller discards those return values intentionally.
			_, _, peerKind, peerRawID, accessHash = extractGlobalChat(msg.PeerID, nil, nil, nil, userByID, channelByID)
			if peerKind != "" {
				return peerKind, peerRawID, accessHash, msg.ID
			}
		case *tg.MessageService:
			if msg.ID <= 0 || msg.PeerID == nil {
				continue
			}
			// Same nil-map rationale as the *tg.Message case above.
			_, _, peerKind, peerRawID, accessHash = extractGlobalChat(msg.PeerID, nil, nil, nil, userByID, channelByID)
			if peerKind != "" {
				return peerKind, peerRawID, accessHash, msg.ID
			}
		}
	}
	return "", 0, 0, 0
}
