package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
			name:      "invalid offset_id",
			input:     SearchMessagesInput{ChatID: 1, Query: "hi", OffsetID: "abc"},
			errSubstr: "invalid message_id",
		},
		{
			name:      "scheduled offset_id rejected",
			input:     SearchMessagesInput{ChatID: 1, Query: "hi", OffsetID: "s:42"},
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
			if err != nil {
				t.Fatalf("handle returned err: %v", err)
			}
			if out != nil {
				t.Fatalf("expected nil output on validation error, got %+v", out)
			}
			if result == nil || !result.IsError {
				t.Fatalf("expected IsError result, got %+v", result)
			}
			text := toolResultText(result)
			if !strings.Contains(text, tc.errSubstr) {
				t.Errorf("result text %q does not contain %q", text, tc.errSubstr)
			}
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
	if len(mediaFilterMap) != len(want) {
		t.Errorf("mediaFilterMap size = %d, want %d", len(mediaFilterMap), len(want))
	}
	for _, k := range want {
		ctor, ok := mediaFilterMap[k]
		if !ok {
			t.Errorf("mediaFilterMap missing %q", k)
			continue
		}
		if ctor() == nil {
			t.Errorf("mediaFilterMap[%q] constructor returned nil", k)
		}
	}
}

// toolResultText extracts the concatenated TextContent from a CallToolResult
// for error-assertion tests. Non-text content is ignored.
func toolResultText(r *mcp.CallToolResult) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
