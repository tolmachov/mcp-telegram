package messages

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"time"

	"github.com/gotd/td/tg"
	"golang.org/x/time/rate"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// Provider fetches messages from Telegram with a unified interface.
type Provider struct {
	client  *tg.Client
	limiter *rate.Limiter
	peers   *tgclient.PeerCache
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
		limiter: rate.NewLimiter(rate.Limit(rps), 1),
		peers:   tgclient.NewPeerCache(),
	}
}

// Fetch retrieves messages from a chat with the given options.
// It handles pagination internally and returns enriched messages with sender names.
func (p *Provider) Fetch(ctx context.Context, chatID int64, opts FetchOptions) (*FetchResult, error) {
	result, err := withPeerRetry(ctx, p, chatID, nil, func(peer tg.InputPeerClass) (*FetchResult, error) {
		return p.fetchWithPeer(ctx, peer, opts)
	})
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
//     strictly older than the cutoff — an exclusive upper bound). Callers
//     wanting an inclusive day must advance the cutoff by 24h themselves
//     (e.g. pass midnight of the next day). Ignored when opts.OffsetDate is
//     already set by the caller (e.g. FetchAll's pagination loop).
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
		historyRequest.OffsetDate = telegramBefore(offsetDate)
	}

	if opts.UnreadOnly && readInboxMaxID > 0 {
		historyRequest.MinID = readInboxMaxID
	}

	if err := p.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("waiting for Telegram rate limit: %w", err)
	}

	history, err := p.client.MessagesGetHistory(ctx, historyRequest)
	if err != nil {
		return nil, fmt.Errorf("getting messages: %w", err)
	}

	result, err := p.processHistory(history, peer, opts.Limit)
	if err != nil {
		return nil, err
	}

	applyMinDateFilter(result, opts.MinDate)

	return result, nil
}

// applyMinDateFilter trims a reverse-chronological page to messages on or
// after minDate. History comes back newest-first, so we walk forward and stop
// at the first message older than minDate — everything from that point on is
// outside the window. When the page is trimmed, HasMore is cleared (the window
// is exhausted) and NextID is re-pointed at the new last message. A zero
// minDate or empty page is a no-op.
func applyMinDateFilter(result *FetchResult, minDate time.Time) {
	if minDate.IsZero() || len(result.Messages) == 0 {
		return
	}
	cutoff := len(result.Messages)
	for i, m := range result.Messages {
		if m.Date.Before(minDate) {
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

// Telegram message dates are whole Unix seconds and its date parameters are
// strict: max_date/offset_date keep dates < bound, min_date keeps dates > bound.
// The tool contract is an inclusive lower bound and an exclusive upper bound
// at any precision, so every bound sent to Telegram goes through these two
// helpers. Rounding up to the next whole second makes them exact for an integer
// date s: s < t ⇔ s < ceil(t), and s >= t ⇔ s > ceil(t)-1.

// telegramBefore converts an exclusive upper bound to max_date/offset_date.
func telegramBefore(t time.Time) int {
	return int(ceilUnix(t))
}

// telegramFromInclusive converts an inclusive lower bound to min_date.
func telegramFromInclusive(t time.Time) int {
	return int(ceilUnix(t) - 1)
}

func ceilUnix(t time.Time) int64 {
	if t.Nanosecond() > 0 {
		return t.Unix() + 1
	}
	return t.Unix()
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
	result, err := withPeerRetry(ctx, p, chatID, nil, func(peer tg.InputPeerClass) (*FetchResult, error) {
		return p.fetchContextWithPeer(ctx, peer, anchorID, before, after)
	})
	if err != nil {
		return nil, err
	}
	result.ChatID = chatID
	return result, nil
}

func (p *Provider) fetchContextWithPeer(ctx context.Context, peer tg.InputPeerClass, anchorID, before, after int) (*FetchResult, error) {

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
		AddOffset: -(after + 1),
		Limit:     limit,
	}

	if err := p.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("waiting for Telegram rate limit: %w", err)
	}
	history, err := p.client.MessagesGetHistory(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("getting message context: %w", err)
	}

	result, err := p.processHistory(history, peer, limit)
	if err != nil {
		return nil, err
	}

	// Flip reverse-chronological → chronological so downstream consumers
	// (and the LLM) see the natural reading order.
	for i, j := 0, len(result.Messages)-1; i < j; i, j = i+1, j-1 {
		result.Messages[i], result.Messages[j] = result.Messages[j], result.Messages[i]
	}

	// Telegram can return overlapping objects around add_offset and service
	// messages can shift the rendered window. Deduplicate, require the anchor,
	// then trim relative to that anchor so the public contract is exact.
	deduped := make([]Message, 0, len(result.Messages))
	seen := make(map[int]struct{}, len(result.Messages))
	anchorIndex := -1
	for _, msg := range result.Messages {
		if _, duplicate := seen[msg.ID]; duplicate {
			continue
		}
		seen[msg.ID] = struct{}{}
		if msg.ID == anchorID {
			anchorIndex = len(deduped)
		}
		deduped = append(deduped, msg)
	}
	if anchorIndex < 0 {
		return nil, fmt.Errorf("anchor message %d was not returned by Telegram", anchorID)
	}
	start := max(0, anchorIndex-before)
	end := min(len(deduped), anchorIndex+after+1)
	result.Messages = deduped[start:end]
	result.Count = len(result.Messages)
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
	result, err := withPeerRetry(ctx, p, chatID, nil, func(peer tg.InputPeerClass) (*FetchResult, error) {
		return p.fetchScheduledWithPeer(ctx, peer)
	})
	if err != nil {
		return nil, err
	}
	result.ChatID = chatID
	return result, nil
}

func (p *Provider) fetchScheduledWithPeer(ctx context.Context, peer tg.InputPeerClass) (*FetchResult, error) {
	if err := p.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("waiting for Telegram rate limit: %w", err)
	}
	history, err := p.client.MessagesGetScheduledHistory(ctx, &tg.MessagesGetScheduledHistoryRequest{
		Peer: peer,
	})
	if err != nil {
		return nil, fmt.Errorf("getting scheduled messages: %w", err)
	}

	// Scheduled messages are unpaginated; limit=0 leaves HasMore false and it
	// is cleared explicitly below regardless.
	result, err := p.processHistory(history, peer, 0)
	if err != nil {
		return nil, err
	}
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
	peer, err := p.peers.Resolve(ctx, p.client, chatID)
	if err != nil {
		return nil, fmt.Errorf("resolving peer: %w", err)
	}

	result, err := p.fetchAllWithPeer(ctx, peer, opts, onBatch)
	if err != nil && tgclient.ShouldRefreshPeer(err) && (result == nil || len(result.Messages) == 0) {
		p.peers.Invalidate(chatID)
		peer, resolveErr := p.peers.Resolve(ctx, p.client, chatID)
		if resolveErr != nil {
			return result, fmt.Errorf("freshly resolving peer: %w", resolveErr)
		}
		result, err = p.fetchAllWithPeer(ctx, peer, opts, onBatch)
	}
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

		if len(batch.Messages) == 0 && !batch.HasMore {
			if onBatch != nil {
				onBatch(batchNum, len(result.Messages), time.Time{})
			}
			break
		}
		if batch.HasMore && (batch.NextID <= 0 || batch.NextID == batchOpts.OffsetID) {
			result.Count = len(result.Messages)
			return result, fmt.Errorf("pagination made no progress after batch %d (cursor %d)", batchNum, batch.NextID)
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
		for messageIndex, msg := range batch.Messages {
			// Check min date filter
			if !opts.MinDate.IsZero() && msg.Date.Before(opts.MinDate) {
				reachedMinDate = true
				break
			}

			result.Messages = append(result.Messages, msg)

			// Check max count
			if opts.MaxCount > 0 && len(result.Messages) >= opts.MaxCount {
				result.Count = len(result.Messages)
				result.HasMore = batch.HasMore || messageIndex < len(batch.Messages)-1
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

// processHistory converts a getHistory/search response into a FetchResult.
// Slice responses continue until Telegram returns an empty raw page. This may
// cost one final empty request, but it cannot truncate short or service-only
// pages. NextID is derived from the raw page, not the rendered DTOs.
func (p *Provider) processHistory(history tg.MessagesMessagesClass, peer tg.InputPeerClass, _ int) (*FetchResult, error) {
	result := &FetchResult{
		Users: make(map[int64]string),
		Chats: make(map[int64]string),
	}

	var messages []tg.MessageClass
	var users []tg.UserClass
	var chats []tg.ChatClass
	hasMore := false

	switch hist := history.(type) {
	case *tg.MessagesMessages:
		// The complete history fit in one response — there is never more to page.
		messages = hist.Messages
		users = hist.Users
		chats = hist.Chats
	case *tg.MessagesMessagesSlice:
		messages = hist.Messages
		users = hist.Users
		chats = hist.Chats
		hasMore = len(hist.Messages) > 0
	case *tg.MessagesChannelMessages:
		messages = hist.Messages
		users = hist.Users
		chats = hist.Chats
		hasMore = len(hist.Messages) > 0
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
	result.RawCount = len(messages)
	result.HasMore = hasMore

	if hasMore {
		result.NextID = lastRawMessageID(messages)
		if result.NextID <= 0 {
			result.HasMore = false
		}
	}

	return result, nil
}

func lastRawMessageID(messages []tg.MessageClass) int {
	for i := len(messages) - 1; i >= 0; i-- {
		switch msg := messages[i].(type) {
		case *tg.Message:
			return msg.ID
		case *tg.MessageService:
			return msg.ID
		case *tg.MessageEmpty:
			return msg.ID
		}
	}
	return 0
}

func (p *Provider) extractMessages(messages []tg.MessageClass, users map[int64]string, chats map[int64]string, peer tg.InputPeerClass) []Message {
	result := make([]Message, 0, len(messages))

	for _, msgClass := range messages {
		msg, ok := msgClass.(*tg.Message)
		if !ok {
			continue
		}
		// In private chats the other user's messages have FromID == nil; the
		// resolved peer is the sender fallback.
		if m, ok := buildMessage(msg, users, chats, peer); ok {
			result = append(result, m)
		}
	}

	return result
}

// buildMessage converts a raw *tg.Message into the enriched domain Message,
// shared by the single-chat history path (extractMessages) and the cross-chat
// search path (processGlobalHistory) so both stay in lockstep. senderFallback
// is the peer treated as the sender when FromID is nil (the DM case): the
// resolved InputPeer for history, or msg.PeerID for global search.
//
// Returns ok=false for non-positive IDs. Telegram guarantees positive IDs, but
// a zero/negative ID would panic FormatRegularRef/FormatScheduledRef at the
// tool boundary, so every path that emits Message must drop them here.
func buildMessage(msg *tg.Message, users, chats map[int64]string, senderFallback any) (Message, bool) {
	if msg.ID <= 0 {
		slog.Warn("buildMessage: dropping message with non-positive ID; Telegram API contract violation", "msg_id", msg.ID)
		return Message{}, false
	}

	m := Message{
		ID:   msg.ID,
		Date: time.Unix(int64(msg.Date), 0),
		Text: msg.Message,
		Raw:  msg,
	}

	if msg.FromID != nil {
		m.SenderID, m.SenderName = extractSender(msg.FromID, users, chats)
	} else {
		m.SenderID, m.SenderName = extractSender(senderFallback, users, chats)
	}
	if msg.ReplyTo != nil {
		m.ReplyToID = extractReplyToID(msg.ReplyTo)
	}
	if msg.Media != nil {
		m.Media = extractMediaType(msg.Media)
	}
	if reactions, ok := msg.GetReactions(); ok {
		m.Reactions = extractReactions(reactions)
	}
	if replies, ok := msg.GetReplies(); ok {
		m.Replies = extractReplies(replies)
	}
	m.Entities = extractURLEntities(msg)

	return m, true
}

// extractURLEntities pulls plain and hidden (text_url) hyperlinks out of a
// message's entities. Telegram encodes entity offsets/lengths in UTF-16 code
// units, so plain URLs are sliced with extractSubstring rather than by byte.
func extractURLEntities(msg *tg.Message) []string {
	var out []string
	for _, entity := range msg.Entities {
		switch e := entity.(type) {
		case *tg.MessageEntityURL:
			if extracted := extractSubstring(msg.Message, e.Offset, e.Length); extracted != "" {
				out = append(out, extracted)
			}
		case *tg.MessageEntityTextURL:
			out = append(out, e.URL)
		}
	}
	return out
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

// extractReactions converts Telegram's aggregated reaction counts into the
// compact ReactionInfo slice. ReactionEmpty and unknown variants are skipped.
func extractReactions(r tg.MessageReactions) []ReactionInfo {
	if len(r.Results) == 0 {
		return nil
	}
	out := make([]ReactionInfo, 0, len(r.Results))
	for _, rc := range r.Results {
		info := ReactionInfo{Count: rc.Count}
		if _, ok := rc.GetChosenOrder(); ok {
			info.Chosen = true
		}
		switch reaction := rc.Reaction.(type) {
		case *tg.ReactionEmoji:
			info.Emoji = reaction.Emoticon
		case *tg.ReactionCustomEmoji:
			info.CustomEmojiID = strconv.FormatInt(reaction.DocumentID, 10)
		case *tg.ReactionPaid:
			info.Paid = true
		default:
			// ReactionEmpty or an unknown variant — nothing to surface.
			continue
		}
		out = append(out, info)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// extractReplies converts Telegram's MessageReplies into the compact
// RepliesInfo. Comments reports whether this is a channel-post comment section
// (true) or a plain group/topic reply thread (false). ChannelID and MaxID are
// optional flag fields, surfaced only when present.
func extractReplies(r tg.MessageReplies) *RepliesInfo {
	info := &RepliesInfo{
		Count:      r.Replies,
		IsComments: r.Comments,
	}
	if id, ok := r.GetChannelID(); ok {
		info.ChannelID = id
	}
	if maxID, ok := r.GetMaxID(); ok {
		info.MaxID = maxID
	}
	return info
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
		slog.Warn("discarding malformed Telegram entity", "offset", offset, "length", length, "text_utf8_bytes", len(s))
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
		slog.Warn("discarding out-of-range Telegram entity", "offset", offset, "length", length, "text_utf8_bytes", len(s))
		return ""
	}

	return string(runes[start:stop])
}
