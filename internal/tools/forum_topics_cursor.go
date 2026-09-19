package tools

import "fmt"

// forumTopicsCursorEnvelope is the wire form of a GetForumTopics pagination
// cursor: the (offset_topic, offset_id, offset_date) tuple Telegram's
// messages.getForumTopics needs to resume from the last topic of the prior
// page, base64(url)-encoded into one opaque string. Field names are short
// because the encoded cursor is copied through tool I/O and counts against
// context on every hop. The version tag lets the decoder reject a newer cursor
// it cannot interpret rather than silently mispaginating.
type forumTopicsCursorEnvelope struct {
	Version     int    `json:"v"`
	ChatID      int64  `json:"c"`
	Query       string `json:"q,omitempty"`
	Limit       int    `json:"l"`
	OffsetTopic int    `json:"ot"`
	OffsetID    int    `json:"oi"`
	OffsetDate  int    `json:"od"`
	Seen        int    `json:"n"`
}

func (e forumTopicsCursorEnvelope) cursorVersion() int { return e.Version }

// Version 3 requires a coherent live-topic anchor and counts only entries
// through that anchor. Version 2 could combine offsets from different topics.
const forumTopicsCursorVersion = 3

// FormatForumTopicsCursor renders the offset tuple as an opaque base64 string.
// Invariant: ParseForumTopicsCursor(FormatForumTopicsCursor(...)) round-trips
// exactly for every tuple produced by the provider.
func FormatForumTopicsCursor(chatID int64, query string, limit, offsetTopic, offsetID, offsetDate, seen int) string {
	env := forumTopicsCursorEnvelope{
		Version:     forumTopicsCursorVersion,
		ChatID:      chatID,
		Query:       query,
		Limit:       limit,
		OffsetTopic: offsetTopic,
		OffsetID:    offsetID,
		OffsetDate:  offsetDate,
		Seen:        seen,
	}
	return encodeCursor(env)
}

// ParseForumTopicsCursor decodes a cursor produced by a prior GetForumTopics
// call into its offset components. Structural validation (base64, JSON,
// version) is enforced here so the LLM gets a descriptive error instead of a
// silent bad-pagination loop.
func ParseForumTopicsCursor(s string) (forumTopicsCursorEnvelope, error) {
	env, err := decodeCursor[forumTopicsCursorEnvelope](s, forumTopicsCursorVersion)
	if err != nil {
		return forumTopicsCursorEnvelope{}, err
	}
	if env.ChatID == 0 || env.Limit <= 0 || env.Limit > 100 {
		return forumTopicsCursorEnvelope{}, fmt.Errorf("cursor contains invalid source filters")
	}
	if env.OffsetTopic <= 0 || env.OffsetID <= 0 || env.OffsetDate <= 0 || env.Seen <= 0 {
		return forumTopicsCursorEnvelope{}, fmt.Errorf("cursor contains invalid pagination anchor")
	}
	return env, nil
}
