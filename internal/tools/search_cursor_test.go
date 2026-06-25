package tools

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/messages"
)

func TestGlobalSearchCursorRoundTrip(t *testing.T) {
	tests := []messages.GlobalSearchCursor{
		{Rate: 123, PeerKind: messages.PeerKindUser, PeerID: 42, AccessHash: 99, MsgID: 7},
		{Rate: 0, PeerKind: messages.PeerKindChat, PeerID: 1001, AccessHash: 0, MsgID: 1},
		{Rate: 456, PeerKind: messages.PeerKindChannel, PeerID: 1234567890, AccessHash: -8, MsgID: 9999},
	}
	for _, c := range tests {
		encoded := FormatGlobalSearchCursor(c)
		require.NotEmpty(t, encoded)
		decoded, err := ParseGlobalSearchCursor(encoded)
		require.NoError(t, err)
		assert.Equal(t, c, *decoded)
	}
}

func TestParseGlobalSearchCursorErrors(t *testing.T) {
	// Pre-compute a valid cursor so we can mutate it into invalid forms
	// without hand-crafting base64 payloads in every test case.
	valid := FormatGlobalSearchCursor(messages.GlobalSearchCursor{
		PeerKind: messages.PeerKindUser, PeerID: 42, MsgID: 7,
	})

	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"empty", "", "empty"},
		{"whitespace", " " + valid, "whitespace"},
		{"not base64", "!!!not-base64!!!", "base64url"},
		{"bad json", "bm90LWpzb24", "JSON"}, // "not-json"
		{"unknown kind", FormatGlobalSearchCursor(messages.GlobalSearchCursor{PeerKind: "bot", PeerID: 1, MsgID: 1}), "peer kind"},
		// Use PeerKind "chat" for the id tests: basic chats have no access_hash
		// requirement, so we can isolate the peer_id / msg_id invariants cleanly.
		{"zero peer id", FormatGlobalSearchCursor(messages.GlobalSearchCursor{PeerKind: messages.PeerKindChat, PeerID: 0, MsgID: 1}), "peer id"},
		{"zero msg id", FormatGlobalSearchCursor(messages.GlobalSearchCursor{PeerKind: messages.PeerKindChat, PeerID: 1, MsgID: 0}), "message id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseGlobalSearchCursor(tc.input)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// encodeRawCursor builds a wire cursor from an arbitrary JSON map so tests
// can construct legacy (V=0, no v field) and future-version cursors that
// FormatGlobalSearchCursor would never emit on its own.
func encodeRawCursor(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshaling test payload: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// TestParseGlobalSearchCursorVersionCompat locks in the version-tolerance
// contract documented on cursorEnvelope: a cursor without a v field
// (legacy, V=0) must decode successfully, a cursor with v > cursorVersion
// must be rejected with a clear "newer than supported" error, and a
// future cursor that adds an unknown field MUST still be decodable when
// v ≤ cursorVersion so forward-compat works.
func TestParseGlobalSearchCursorVersionCompat(t *testing.T) {
	t.Run("legacy cursor without v field", func(t *testing.T) {
		input := encodeRawCursor(t, map[string]any{
			"r": 42,
			"k": "user",
			"i": 100,
			"h": 999,
			"m": 7,
		})
		got, err := ParseGlobalSearchCursor(input)
		require.NoError(t, err)
		assert.Equal(t, messages.PeerKindUser, got.PeerKind)
		assert.Equal(t, int64(100), got.PeerID)
		assert.Equal(t, 7, got.MsgID)
	})

	t.Run("future cursor with higher version", func(t *testing.T) {
		input := encodeRawCursor(t, map[string]any{
			"v": 999,
			"k": "user",
			"i": 1,
			"h": 2,
			"m": 1,
		})
		_, err := ParseGlobalSearchCursor(input)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "newer than supported")
	})

	t.Run("cursor with unknown field at current version", func(t *testing.T) {
		// Forward-compat: a cursor with v=1 but an extra field (as if a
		// future minor-compat bump added an optional key) must still
		// decode, not fail with "unknown field".
		input := encodeRawCursor(t, map[string]any{
			"v":               1,
			"k":               "user",
			"i":               1,
			"h":               2,
			"m":               1,
			"future_optional": "ignored",
		})
		_, err := ParseGlobalSearchCursor(input)
		assert.NoError(t, err)
	})
}
