package tools

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// chatsCursorEnvelope is the wire form of a GetChats pagination cursor.
// Field names are kept short to minimise LLM token usage.
type chatsCursorEnvelope struct {
	V int   `json:"v"` // schema version
	S int64 `json:"s"` // cache session ID
	O int   `json:"o"` // offset into cached slice
}

const chatsCursorVersion = 1

// FormatChatsCursor encodes a pagination cursor as an opaque base64url string.
// Panics on negative offset — callers must ensure offset >= 0.
func FormatChatsCursor(sessionID int64, offset int) string {
	if offset < 0 {
		panic(fmt.Sprintf("FormatChatsCursor: negative offset %d", offset))
	}
	env := chatsCursorEnvelope{V: chatsCursorVersion, S: sessionID, O: offset}
	payload, err := json.Marshal(env)
	if err != nil {
		panic(fmt.Sprintf("marshaling chats cursor: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

// ParseChatsCursor decodes an opaque cursor string produced by a prior
// GetChats call. Returns the cache session ID and offset, or an error.
func ParseChatsCursor(s string) (sessionID int64, offset int, err error) {
	if s == "" {
		return 0, 0, fmt.Errorf("cursor is empty")
	}
	if s != strings.TrimSpace(s) {
		return 0, 0, fmt.Errorf("cursor contains whitespace")
	}

	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, 0, fmt.Errorf("cursor is not valid base64url: %w", err)
	}

	var env chatsCursorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, 0, fmt.Errorf("cursor payload is not valid JSON: %w", err)
	}

	if env.V < 1 || env.V > chatsCursorVersion {
		return 0, 0, fmt.Errorf("cursor version %d is not supported (expected %d); restart pagination without cursor", env.V, chatsCursorVersion)
	}
	if env.O < 0 {
		return 0, 0, fmt.Errorf("cursor offset %d is negative", env.O)
	}

	return env.S, env.O, nil
}
