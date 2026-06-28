package tools

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

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
	payload, err := json.Marshal(env)
	if err != nil {
		// A fixed struct of ints cannot fail to marshal; if it ever does,
		// silently returning an empty cursor would mask the bug.
		panic(fmt.Sprintf("marshaling forum topics cursor envelope: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

// ParseForumTopicsCursor decodes a cursor produced by a prior GetForumTopics
// call into its offset components. Structural validation (base64, JSON,
// version) is enforced here so the LLM gets a descriptive error instead of a
// silent bad-pagination loop.
func ParseForumTopicsCursor(s string) (offsetTopic, offsetID, offsetDate int, err error) {
	if s == "" {
		return 0, 0, 0, fmt.Errorf("cursor is empty")
	}
	if s != strings.TrimSpace(s) {
		return 0, 0, 0, fmt.Errorf("cursor contains whitespace")
	}

	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("cursor is not valid base64url: %w", err)
	}

	// We intentionally do NOT use DisallowUnknownFields: the Version field is
	// the schema-compat mechanism. A future v1-compatible cursor that adds an
	// optional key must still decode here, so the version check below — not a
	// strict-field decoder — is what guards against incompatible cursors.
	var env forumTopicsCursorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, 0, 0, fmt.Errorf("cursor payload is not valid JSON: %w", err)
	}
	if env.Version > forumTopicsCursorVersion {
		return 0, 0, 0, fmt.Errorf("cursor version %d is newer than supported (%d); restart pagination without the cursor", env.Version, forumTopicsCursorVersion)
	}

	return env.OffsetTopic, env.OffsetID, env.OffsetDate, nil
}
