package messages

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/gotd/td/tg"
)

// ForumTopic is a single thematic thread ("topic") of a forum supergroup, as
// returned by messages.getForumTopics. ID doubles as the root message ID used
// to fetch the topic's messages via FetchReplies.
type ForumTopic struct {
	ID           int       `json:"id"`
	Title        string    `json:"title"`
	TopMessageID int       `json:"top_message_id,omitempty"` // ID of the last message in the topic
	UnreadCount  int       `json:"unread_count,omitempty"`
	Closed       bool      `json:"closed,omitempty"`
	Pinned       bool      `json:"pinned,omitempty"`
	Hidden       bool      `json:"hidden,omitempty"` // only the General topic can be hidden
	IconColor    int       `json:"icon_color,omitempty"`
	IconEmojiID  string    `json:"icon_emoji_id,omitempty"` // custom emoji document ID (int64 as string)
	Date         time.Time `json:"date"`
}

// ForumTopicsOffset is the (offset_topic, offset_id, offset_date) tuple
// Telegram's getForumTopics needs to resume from the last topic of a page. The
// three components are only meaningful together, so they are bundled into one
// value — a nil *ForumTopicsOffset means "no more pages", which makes a
// half-built or all-zero offset unrepresentable. Mirrors the nil-when-done
// shape of GlobalSearchResult.NextCursor.
type ForumTopicsOffset struct {
	Topic int
	ID    int
	Date  int
}

// ForumTopicsResult is one page of forum topics. NextOffset is non-nil exactly
// when another page exists; the tool layer packs it into an opaque cursor.
type ForumTopicsResult struct {
	Topics     []ForumTopic
	Count      int                // total topics reported by Telegram
	NextOffset *ForumTopicsOffset // nil = no more pages
}

// FetchForumTopics lists the topics of a forum supergroup via
// messages.getForumTopics. query filters by topic title (empty = all topics).
// Pass zero offsets for the first page; for subsequent pages pass the
// NextOffset* values from the previous result.
//
// Telegram only accepts this call for forum-enabled supergroups; for any other
// peer it returns an error, which is propagated to the caller.
func (p *Provider) FetchForumTopics(ctx context.Context, chatID int64, query string, limit, offsetTopic, offsetID, offsetDate int) (*ForumTopicsResult, error) {
	if limit <= 0 {
		limit = 100
	}

	peer, err := p.peers.Resolve(ctx, p.client, chatID)
	if err != nil {
		return nil, fmt.Errorf("resolving peer: %w", err)
	}

	req := &tg.MessagesGetForumTopicsRequest{
		Peer:        peer,
		Limit:       limit,
		OffsetTopic: offsetTopic,
		OffsetID:    offsetID,
		OffsetDate:  offsetDate,
	}
	if query != "" {
		req.SetQ(query)
	}

	p.limiter.Take()

	resp, err := p.client.MessagesGetForumTopics(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("getting forum topics: %w", err)
	}

	return buildForumTopicsResult(resp, limit), nil
}

// buildForumTopicsResult converts a messages.getForumTopics response into a
// paginated ForumTopicsResult. Split out from FetchForumTopics so the parsing,
// HasMore computation, and next-offset derivation are testable without a live
// client (mirroring processHistory).
func buildForumTopicsResult(resp *tg.MessagesForumTopics, limit int) *ForumTopicsResult {
	// Index related messages by ID so we can recover each topic's
	// top-message date for the pagination cursor.
	msgDates := make(map[int]int, len(resp.Messages))
	for _, mc := range resp.Messages {
		if m, ok := mc.(*tg.Message); ok {
			msgDates[m.ID] = m.Date
		}
	}

	result := &ForumTopicsResult{
		Topics: make([]ForumTopic, 0, len(resp.Topics)),
		Count:  resp.Count,
	}

	var lastTopic *tg.ForumTopic
	for _, tc := range resp.Topics {
		topic, ok := tc.(*tg.ForumTopic)
		if !ok {
			// *tg.ForumTopicDeleted carries only an ID — nothing to render.
			continue
		}
		ft := ForumTopic{
			ID:           topic.ID,
			Title:        topic.Title,
			TopMessageID: topic.TopMessage,
			UnreadCount:  topic.UnreadCount,
			Closed:       topic.Closed,
			Pinned:       topic.Pinned,
			Hidden:       topic.Hidden,
			IconColor:    topic.IconColor,
			Date:         time.Unix(int64(topic.Date), 0),
		}
		if emojiID, ok := topic.GetIconEmojiID(); ok {
			ft.IconEmojiID = strconv.FormatInt(emojiID, 10)
		}
		result.Topics = append(result.Topics, ft)
		lastTopic = topic
	}

	// More pages remain only when the raw page came back full
	// (len(resp.Topics) >= limit), the rendered topics still fall short of the
	// reported total, AND there is a real topic to anchor the next offset on.
	// The lastTopic != nil guard is load-bearing: a full page consisting only of
	// ForumTopicDeleted entries has nothing to advance the cursor past, and a
	// zero offset would restart pagination from the first page forever. In that
	// (rare) pathological case we stop early rather than loop.
	if len(resp.Topics) >= limit && len(result.Topics) < result.Count && lastTopic != nil {
		offset := &ForumTopicsOffset{
			Topic: lastTopic.ID,
			ID:    lastTopic.TopMessage,
		}
		if d, ok := msgDates[lastTopic.TopMessage]; ok {
			offset.Date = d
		} else {
			// The last topic's top message wasn't in resp.Messages (e.g. it was
			// deleted); fall back to the topic creation date. It can differ from
			// the message date, so log it — a wrong offset_date can skip or
			// duplicate topics and would otherwise be invisible.
			offset.Date = lastTopic.Date
			slog.Warn("buildForumTopicsResult: top message missing for last topic; using topic date as pagination offset_date (cursor may be approximate)",
				"topic_id", lastTopic.ID, "top_message", lastTopic.TopMessage)
		}
		result.NextOffset = offset
	}

	return result
}
