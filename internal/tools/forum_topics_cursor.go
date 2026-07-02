package tools

// forumTopicsCursorEnvelope is the wire form of a GetForumTopics pagination
// cursor: the (offset_topic, offset_id, offset_date) tuple Telegram's
// messages.getForumTopics needs to resume from the last topic of the prior
// page, base64(url)-encoded into one opaque string. Field names are short
// because the encoded cursor is copied through tool I/O and counts against
// context on every hop. The version tag lets the decoder reject a newer cursor
// it cannot interpret rather than silently mispaginating.
type forumTopicsCursorEnvelope struct {
	Version     int `json:"v"`
	OffsetTopic int `json:"ot"`
	OffsetID    int `json:"oi"`
	OffsetDate  int `json:"od"`
}

func (e forumTopicsCursorEnvelope) cursorVersion() int { return e.Version }

// forumTopicsCursorVersion is the current envelope schema version.
const forumTopicsCursorVersion = 1

// FormatForumTopicsCursor renders the offset tuple as an opaque base64 string.
// Invariant: ParseForumTopicsCursor(FormatForumTopicsCursor(...)) round-trips
// exactly for every tuple produced by the provider.
func FormatForumTopicsCursor(offsetTopic, offsetID, offsetDate int) string {
	env := forumTopicsCursorEnvelope{
		Version:     forumTopicsCursorVersion,
		OffsetTopic: offsetTopic,
		OffsetID:    offsetID,
		OffsetDate:  offsetDate,
	}
	return encodeCursor(env)
}

// ParseForumTopicsCursor decodes a cursor produced by a prior GetForumTopics
// call into its offset components. Structural validation (base64, JSON,
// version) is enforced here so the LLM gets a descriptive error instead of a
// silent bad-pagination loop.
func ParseForumTopicsCursor(s string) (offsetTopic, offsetID, offsetDate int, err error) {
	env, err := decodeCursor[forumTopicsCursorEnvelope](s, forumTopicsCursorVersion)
	if err != nil {
		return 0, 0, 0, err
	}

	return env.OffsetTopic, env.OffsetID, env.OffsetDate, nil
}
