package tools

import (
	"context"
	"strings"
	"testing"
)

// As with TestSearchMessagesHandleValidation, these tests only touch the
// pre-provider validation path, so a nil provider is safe.
func TestSearchMessagesGlobalHandleValidation(t *testing.T) {
	h := NewMessagesSearchGlobalHandler(nil)
	ctx := context.Background()

	tests := []struct {
		name      string
		input     SearchMessagesGlobalInput
		errSubstr string
	}{
		{
			name:      "missing query",
			input:     SearchMessagesGlobalInput{},
			errSubstr: "query is required",
		},
		{
			name:      "query whitespace only",
			input:     SearchMessagesGlobalInput{Query: "   "},
			errSubstr: "query is required",
		},
		{
			name:      "invalid cursor",
			input:     SearchMessagesGlobalInput{Query: "hi", Cursor: "!!!not-a-cursor!!!"},
			errSubstr: "invalid cursor",
		},
		{
			name:      "bad from_date",
			input:     SearchMessagesGlobalInput{Query: "hi", FromDate: "yesterday"},
			errSubstr: "invalid from_date",
		},
		{
			name:      "bad to_date",
			input:     SearchMessagesGlobalInput{Query: "hi", ToDate: "next week"},
			errSubstr: "invalid to_date",
		},
		{
			name: "from_date after to_date",
			input: SearchMessagesGlobalInput{
				Query:    "hi",
				FromDate: "2026-12-31T00:00:00Z",
				ToDate:   "2026-01-01T00:00:00Z",
			},
			errSubstr: "window is empty",
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
			if text := toolResultText(result); !strings.Contains(text, tc.errSubstr) {
				t.Errorf("result text %q does not contain %q", text, tc.errSubstr)
			}
		})
	}
}
