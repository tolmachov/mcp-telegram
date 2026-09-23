package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRepliesGetHandlerValidation covers GetReplies input validation. The
// handler uses a nil provider, safe because every case returns before any
// Telegram API call.
func TestRepliesGetHandlerValidation(t *testing.T) {
	h := NewGetRepliesHandler(nil)
	ctx := context.Background()

	cases := []struct {
		name        string
		in          GetRepliesInput
		wantErrPart string
	}{
		{"zero chat_id", GetRepliesInput{MessageID: "1"}, "chat_id is required"},
		{"empty message_id", GetRepliesInput{ChatID: 1}, "invalid message_id"},
		{"non-numeric message_id", GetRepliesInput{ChatID: 1, MessageID: "abc"}, "invalid message_id"},
		{"scheduled message_id", GetRepliesInput{ChatID: 1, MessageID: "s:42"}, "cannot read replies to a scheduled message"},
		{"non-numeric before_message_id", GetRepliesInput{ChatID: 1, MessageID: "1", BeforeMessageID: "xx"}, "invalid before_message_id"},
		{"scheduled before_message_id", GetRepliesInput{ChatID: 1, MessageID: "1", BeforeMessageID: "s:5"}, "cannot page before a scheduled message"},
		{"bad from_date", GetRepliesInput{ChatID: 1, MessageID: "1", FromDate: "nope"}, "invalid from_date"},
		{"from after to", GetRepliesInput{ChatID: 1, MessageID: "1", FromDate: "2026-02-01T00:00:00Z", ToDate: "2026-01-01T00:00:00Z"}, "window is empty"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRes, out, err := h.handle(ctx, nil, tc.in)
			require.NoError(t, err)
			require.Nil(t, out)
			require.NotNil(t, errRes)
			require.True(t, errRes.IsError)
			assert.Contains(t, toolResultText(errRes), tc.wantErrPart)
		})
	}
}

func TestForumTopicsGetHandlerValidation(t *testing.T) {
	h := NewGetForumTopicsHandler(nil)
	ctx := context.Background()

	cases := []struct {
		name        string
		in          GetForumTopicsInput
		wantErrPart string
	}{
		{"zero chat_id", GetForumTopicsInput{}, "chat_id is required"},
		{"invalid cursor", GetForumTopicsInput{Cursor: "!!!"}, "invalid cursor"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRes, out, err := h.handle(ctx, nil, tc.in)
			require.NoError(t, err)
			require.Nil(t, out)
			require.NotNil(t, errRes)
			require.True(t, errRes.IsError)
			assert.Contains(t, toolResultText(errRes), tc.wantErrPart)
		})
	}
}
