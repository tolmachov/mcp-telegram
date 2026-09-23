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
	if err := checkDateWindow(opts.MinDate, opts.MaxDate); err != nil {
		return nil, err
	}

	// The sender filter is a peer too, so it is resolved (and refreshed on a
	// stale hash) together with the chat.
	ids := []int64{chatID}
	if opts.FromSenderID != 0 {
		ids = append(ids, opts.FromSenderID)
	}
	return tgclient.WithPeers(ctx, p.peers, ids, func(peers []tgclient.Peer) (*FetchResult, error) {
		var sender *tgclient.Peer
		if len(peers) > 1 {
			sender = &peers[1]
		}
		return p.searchWithPeer(ctx, chatID, peers[0].Input, sender, opts)
	})
}

// checkDateWindow rejects a window whose lower bound is not before its upper
// bound; a zero bound is open.
func checkDateWindow(minDate, maxDate time.Time) error {
	if !minDate.IsZero() && !maxDate.IsZero() && !minDate.Before(maxDate) {
		return fmt.Errorf("min_date (%s) is not before max_date (%s): the date window is empty",
			minDate.Format(time.RFC3339), maxDate.Format(time.RFC3339))
	}
	return nil
}

// searchWithPeer runs the search in peer; sender, when non-nil, is the
// resolved opts.FromSenderID.
func (p *Provider) searchWithPeer(ctx context.Context, chatID int64, peer tg.InputPeerClass, sender *tgclient.Peer, opts SearchOptions) (*FetchResult, error) {
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
		req.MinDate = telegramFromInclusive(opts.MinDate)
	}
	if !opts.MaxDate.IsZero() {
		req.MaxDate = telegramBefore(opts.MaxDate)
	}
	if opts.TopMsgID > 0 {
		req.SetTopMsgID(opts.TopMsgID)
	}
	if sender != nil {
		// messages.search from_id only accepts user or channel peers.
		// Basic-chat peers (legacy InputPeerChat) are silently dropped by
		// Telegram, turning what the caller framed as a sender filter into
		// an un-filtered search. Reject loudly instead.
		if _, isBasicChat := sender.Input.(*tg.InputPeerChat); isBasicChat {
			return nil, fmt.Errorf("from_sender_id %d resolves to a legacy basic chat, which cannot be used as a message sender; pass a user ID or a channel/supergroup ID", opts.FromSenderID)
		}
		req.SetFromID(sender.Input)
	}

	if err := p.wait(ctx); err != nil {
		return nil, err
	}

	history, err := p.client.MessagesSearch(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("searching messages: %w", err)
	}

	result := p.processHistory(history, peer)
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
	if err := checkDateWindow(opts.MinDate, opts.MaxDate); err != nil {
		return nil, err
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
		req.MinDate = telegramFromInclusive(opts.MinDate)
	}
	if !opts.MaxDate.IsZero() {
		req.MaxDate = telegramBefore(opts.MaxDate)
	}

	if err := p.wait(ctx); err != nil {
		return nil, err
	}

	history, err := p.client.MessagesSearchGlobal(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("global searching messages: %w", err)
	}

	return p.processGlobalHistory(history)
}

// processGlobalHistory converts a cross-chat search response into per-chat
// attributed messages. Unlike processHistory (which knows the single owning
// peer up front), this walks msg.PeerID for each message to determine the
// chat it belongs to and look up the display title. It also extracts the
// next pagination cursor from the last message when the response is a slice
// and exposes next_rate.
func (p *Provider) processGlobalHistory(history tg.MessagesMessagesClass) (*GlobalSearchResult, error) {
	hist, ok := history.AsModified()
	if !ok {
		// Telegram signals "nothing changed" — return an empty result so the
		// caller sees zero matches rather than a hard error.
		return &GlobalSearchResult{Messages: make([]GlobalMessage, 0)}, nil
	}
	rawMessages := hist.GetMessages()
	_, complete := hist.(*tg.MessagesMessages)
	paginatable := !complete
	nextRate := 0
	if slice, ok := hist.(*tg.MessagesMessagesSlice); ok {
		nextRate, _ = slice.GetNextRate()
	}

	// Build per-ID maps once so extractGlobalChat is O(1) per message instead
	// of O(users+chats). The name maps give display titles keyed by the bare
	// Telegram ID; userByID / channelByID hold full records so access_hash
	// lookups for cursor construction are O(1) too.
	users, chats := tg.UserClassArray(hist.GetUsers()), tg.ChatClassArray(hist.GetChats())
	userMap, chatTitleMap := nameMaps(users, chats)
	userByID := users.UserToMap()
	channelByID := chats.ChannelToMap()

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
		chatID, chatTitle, peerKind, _ := extractGlobalChat(msg.PeerID, userMap, chatTitleMap, userByID, channelByID)
		if peerKind == "" {
			slog.Debug("SearchMessagesGlobal: skipping message with unknown peer kind", "msg_id", msg.ID, "peer_type", fmt.Sprintf("%T", msg.PeerID))
			skipped++
			continue
		}

		// DM fallback: sender is the other end of the conversation (msg.PeerID).
		m, ok := buildMessage(msg, userMap, chatTitleMap, msg.PeerID)
		if !ok {
			skipped++
			continue
		}

		result.Messages = append(result.Messages, GlobalMessage{
			Message:   m,
			ChatID:    chatID,
			ChatTitle: chatTitle,
		})
	}

	result.Count = len(result.Messages)
	result.SkippedCount = skipped

	// Build the next cursor for every non-empty slice response when we have a
	// last-message anchor. Gating on the *raw* page length (not the filtered
	// result) matters: Telegram may return MessageService / MessageEmpty
	// slots that we drop above, so filtering could otherwise make a full
	// page look partial and silently end pagination mid-stream. next_rate is
	// optional and defaults to 0 when absent.
	if paginatable && len(rawMessages) > 0 {
		lastPeerKind, lastPeerID, lastAccessHash, lastMsgID := lastGlobalCursorAnchor(rawMessages, userByID, channelByID)
		if lastPeerKind == "" || lastMsgID <= 0 {
			return result, nil
		}
		cursor, err := NewGlobalSearchCursor(nextRate, lastPeerKind, lastPeerID, lastAccessHash, lastMsgID)
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
//	chatID         — bare MTProto id (positive for every peer type); this is
//	                 also the id fed back into InputPeer reconstruction
//	chatTitle      — display name for UI
//	peerKind       — "user" | "chat" | "channel" (drives cursor round-trip)
//	accessHash     — required for user/channel InputPeer; 0 for basic chat
//
// The userByID / channelByID maps are pre-built by the caller so access_hash
// lookups are O(1) instead of a linear scan per message.
func extractGlobalChat(
	peer tg.PeerClass,
	users map[int64]string,
	chatTitles map[int64]string,
	userByID map[int64]*tg.User,
	channelByID map[int64]*tg.Channel,
) (chatID int64, chatTitle string, peerKind PeerKind, accessHash int64) {
	switch p := peer.(type) {
	case *tg.PeerUser:
		peerKind = PeerKindUser
		chatID = p.UserID
		chatTitle = users[p.UserID]
		if user, ok := userByID[p.UserID]; ok {
			accessHash = user.AccessHash
		}
	case *tg.PeerChat:
		peerKind = PeerKindChat
		chatID = p.ChatID
		chatTitle = chatTitles[p.ChatID]
	case *tg.PeerChannel:
		peerKind = PeerKindChannel
		chatID = p.ChannelID
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
		msg, ok := rawMessages[i].AsNotEmpty()
		if !ok || msg.GetID() <= 0 || msg.GetPeerID() == nil {
			continue
		}
		// nil name maps are safe: only peerKind, chatID (the bare id), and
		// accessHash feed cursor construction; the title lookup is discarded.
		peerRawID, _, peerKind, accessHash = extractGlobalChat(msg.GetPeerID(), nil, nil, userByID, channelByID)
		if peerKind != "" {
			return peerKind, peerRawID, accessHash, msg.GetID()
		}
	}
	return "", 0, 0, 0
}
