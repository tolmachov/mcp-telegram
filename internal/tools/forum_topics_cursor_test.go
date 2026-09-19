package tools

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForumTopicsCursorRoundTrip(t *testing.T) {
	cases := [][4]int{
		{7, 510, 1717000000, 100},
		{0, 0, 0, 0},
		{1, 2, 3, 4},
	}
	for _, c := range cases {
		encoded := FormatForumTopicsCursor(123, "query", 50, c[0], c[1], c[2], c[3])
		require.NotEmpty(t, encoded)
		cursor, err := ParseForumTopicsCursor(encoded)
		require.NoError(t, err)
		assert.Equal(t, int64(123), cursor.ChatID)
		assert.Equal(t, "query", cursor.Query)
		assert.Equal(t, 50, cursor.Limit)
		assert.Equal(t, c[0], cursor.OffsetTopic)
		assert.Equal(t, c[1], cursor.OffsetID)
		assert.Equal(t, c[2], cursor.OffsetDate)
		assert.Equal(t, c[3], cursor.Seen)
	}
}

func TestParseForumTopicsCursorErrors(t *testing.T) {
	valid := FormatForumTopicsCursor(123, "", 50, 7, 510, 100, 3)

	cases := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"empty", "", "empty"},
		{"whitespace", " " + valid, "whitespace"},
		{"not base64", "!!!not-base64!!!", "base64url"},
		{"bad json", "bm90LWpzb24", "JSON"}, // "not-json"
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseForumTopicsCursor(tc.input)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestParseForumTopicsCursorVersionTooNew(t *testing.T) {
	raw, err := json.Marshal(map[string]any{"v": 999, "ot": 1, "oi": 2, "od": 3})
	require.NoError(t, err)
	encoded := base64.RawURLEncoding.EncodeToString(raw)

	_, err = ParseForumTopicsCursor(encoded)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}
