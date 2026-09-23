package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// All test cases here exercise only the input-validation paths of the
// SearchMessages handler — those return before reaching the provider, so
// a nil-provider handler is safe. Any test case that would reach the
// provider must be added to an integration test against a live client.
func TestSearchMessagesHandleValidation(t *testing.T) {
	h := NewMessagesSearchHandler(nil)
	ctx := context.Background()

	tests := []struct {
		name      string
		input     SearchMessagesInput
		errSubstr string
	}{
		{
			name:      "missing chat_id",
			input:     SearchMessagesInput{Query: "hi"},
			errSubstr: "chat_id is required",
		},
		{
			name:      "missing query",
			input:     SearchMessagesInput{ChatID: 1},
			errSubstr: "query is required",
		},
		{
			name:      "query whitespace only",
			input:     SearchMessagesInput{ChatID: 1, Query: "   "},
			errSubstr: "query is required",
		},
		{
			name:      "invalid before_message_id",
			input:     SearchMessagesInput{ChatID: 1, Query: "hi", BeforeMessageID: "abc"},
			errSubstr: "invalid before_message_id",
		},
		{
			name:      "scheduled before_message_id rejected",
			input:     SearchMessagesInput{ChatID: 1, Query: "hi", BeforeMessageID: "s:42"},
			errSubstr: "scheduled",
		},
		{
			name:      "invalid top_msg_id",
			input:     SearchMessagesInput{ChatID: 1, Query: "hi", TopMsgID: "nope"},
			errSubstr: "invalid top_msg_id",
		},
		{
			name:      "scheduled top_msg_id rejected",
			input:     SearchMessagesInput{ChatID: 1, Query: "hi", TopMsgID: "s:1"},
			errSubstr: "scheduled",
		},
		{
			name:      "bad from_date",
			input:     SearchMessagesInput{ChatID: 1, Query: "hi", FromDate: "yesterday"},
			errSubstr: "invalid from_date",
		},
		{
			name:      "bad to_date",
			input:     SearchMessagesInput{ChatID: 1, Query: "hi", ToDate: "tomorrow"},
			errSubstr: "invalid to_date",
		},
		{
			name: "from_date after to_date",
			input: SearchMessagesInput{
				ChatID: 1, Query: "hi",
				FromDate: "2026-12-31T00:00:00Z",
				ToDate:   "2026-01-01T00:00:00Z",
			},
			errSubstr: "window is empty",
		},
		{
			name:      "unknown media_type",
			input:     SearchMessagesInput{ChatID: 1, Query: "hi", MediaType: "emoji"},
			errSubstr: "invalid media_type",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, out, err := h.handle(ctx, nil, tc.input)
			require.NoError(t, err)
			require.Nil(t, out)
			require.NotNil(t, result)
			require.True(t, result.IsError)
			assert.Contains(t, toolResultText(result), tc.errSubstr)
		})
	}
}

func TestMediaFilterMapCoverage(t *testing.T) {
	// Keeps the whitelist in sync with the jsonschema description so
	// future additions have to update both sides deliberately.
	want := []string{
		"photos", "videos", "documents", "links",
		"voice", "music", "gif", "round_video", "round_voice",
	}
	assert.Equal(t, len(want), len(mediaFilterMap))
	for _, k := range want {
		ctor, ok := mediaFilterMap[k]
		require.True(t, ok, "mediaFilterMap missing %q", k)
		require.NotNil(t, ctor())
	}
}
