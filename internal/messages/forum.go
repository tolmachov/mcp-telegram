package messages

import (
	"context"
	"fmt"
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
	Seen  int
}

// ForumTopicsResult is one page of forum topics. NextOffset is non-nil exactly
// when another page exists; the tool layer packs it into an opaque cursor.
type ForumTopicsResult struct {
	Topics     []ForumTopic
	Count      int                // rendered topics in this page
	Total      int                // total raw topics reported by Telegram
	RawCount   int                // raw topics consumed, including deleted entries
	NextOffset *ForumTopicsOffset // nil = no more pages
}

// FetchForumTopics lists the topics of a forum supergroup via
// messages.getForumTopics. query filters by topic title (empty = all topics).
// Pass zero offsets for the first page; for subsequent pages pass the
// NextOffset* values from the previous result.
//
// Telegram only accepts this call for forum-enabled supergroups; for any other
// peer it returns an error, which is propagated to the caller.
func (p *Provider) FetchForumTopics(ctx context.Context, chatID int64, query string, limit, offsetTopic, offsetID, offsetDate, seen int) (*ForumTopicsResult, error) {
	return withPeerRetry(ctx, p, chatID, nil, nil, func(peer tg.InputPeerClass) (*ForumTopicsResult, error) {
		return p.fetchForumTopicsWithPeer(ctx, peer, query, limit, offsetTopic, offsetID, offsetDate, seen)
	})
}

func (p *Provider) fetchForumTopicsWithPeer(ctx context.Context, peer tg.InputPeerClass, query string, limit, offsetTopic, offsetID, offsetDate, seen int) (*ForumTopicsResult, error) {
	if limit <= 0 {
		limit = 100
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

	if err := p.wait(ctx); err != nil {
		return nil, err
	}

	resp, err := p.client.MessagesGetForumTopics(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("getting forum topics: %w", err)
	}

	// Check before using Count: a repeated page must not look terminal just
	// because its entries were counted twice.
	if offsetTopic > 0 {
		for i := len(resp.Topics) - 1; i >= 0; i-- {
			if topic, ok := resp.Topics[i].(*tg.ForumTopic); ok {
				if topic.ID == offsetTopic {
					return nil, fmt.Errorf("forum pagination did not advance past topic %d", offsetTopic)
				}
				break
			}
		}
	}
	return buildForumTopicsResult(resp, seen)
}

// buildForumTopicsResult converts a messages.getForumTopics response into a
// paginated ForumTopicsResult. Split out from FetchForumTopics so the parsing,
// HasMore computation, and next-offset derivation are testable without a live
// client (mirroring processHistory).
func buildForumTopicsResult(resp *tg.MessagesForumTopics, seen int) (*ForumTopicsResult, error) {
	// Index related messages by ID so we can recover each topic's
	// top-message date for the pagination cursor.
	msgDates := make(map[int]int, len(resp.Messages))
	for _, mc := range resp.Messages {
		if m, ok := mc.AsNotEmpty(); ok {
			msgDates[m.GetID()] = m.GetDate()
		}
	}

	result := &ForumTopicsResult{
		Topics:   make([]ForumTopic, 0, len(resp.Topics)),
		Total:    resp.Count,
		RawCount: len(resp.Topics),
	}

	var lastTopic *tg.ForumTopic
	var anchorSeen int
	for i, tc := range resp.Topics {
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
		anchorSeen = seen + i + 1
	}
	result.Count = len(result.Topics)

	// Telegram can return a short page before the end. Deleted topics have no
	// message/date anchor, so resume after the last live topic and count only
	// entries through that anchor. Trailing tombstones may be returned again.
	if len(resp.Topics) > 0 && seen+len(resp.Topics) < resp.Count {
		if lastTopic == nil {
			return nil, fmt.Errorf("cannot paginate forum topics: page contains only deleted topics")
		}
		date := msgDates[lastTopic.TopMessage]
		if resp.OrderByCreateDate {
			date = lastTopic.Date
		}
		if lastTopic.ID <= 0 || lastTopic.TopMessage <= 0 || date <= 0 {
			return nil, fmt.Errorf("cannot paginate forum topics: missing anchor data for topic %d", lastTopic.ID)
		}
		result.NextOffset = &ForumTopicsOffset{
			Topic: lastTopic.ID,
			ID:    lastTopic.TopMessage,
			Date:  date,
			Seen:  anchorSeen,
		}
	}

	return result, nil
}
