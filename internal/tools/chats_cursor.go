package tools

import (
	"fmt"
)

// chatsCursorEnvelope is the wire form of a GetChats pagination cursor.
// Field names are kept short to minimise LLM token usage.
type chatsCursorEnvelope struct {
	V int   `json:"v"` // schema version
	S int64 `json:"s"` // cache session ID
	O int   `json:"o"` // offset into cached slice
}

func (e chatsCursorEnvelope) cursorVersion() int { return e.V }

const chatsCursorVersion = 1

// FormatChatsCursor encodes a pagination cursor as an opaque base64url string.
// Panics on negative offset — callers must ensure offset >= 0.
func FormatChatsCursor(sessionID int64, offset int) string {
	if offset < 0 {
		panic(fmt.Sprintf("FormatChatsCursor: negative offset %d", offset))
	}
	return encodeCursor(chatsCursorEnvelope{V: chatsCursorVersion, S: sessionID, O: offset})
}

// ParseChatsCursor decodes an opaque cursor string produced by a prior
// GetChats call. Returns the cache session ID and offset, or an error. A stale
// session ID is caught by the caller: it no longer matches the live cache, so
// the caller rejects it with a cursor-expired error telling the client to
// restart pagination. Structural validity is all this needs to enforce.
func ParseChatsCursor(s string) (sessionID int64, offset int, err error) {
	env, err := decodeCursor[chatsCursorEnvelope](s, chatsCursorVersion)
	if err != nil {
		return 0, 0, err
	}
	if env.O < 0 {
		return 0, 0, fmt.Errorf("cursor offset %d is negative", env.O)
	}

	return env.S, env.O, nil
}
