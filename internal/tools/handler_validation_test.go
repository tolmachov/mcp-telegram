package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMessageSendHandlerValidation covers the input-validation layer of
// MessageSendHandler.handle. The handler uses a nil *tg.Client, which is safe
// here because every test case is expected to return before any Telegram API
// call is made.
func TestMessageSendHandlerValidation(t *testing.T) {
	h := &MessageSendHandler{}
	ctx := context.Background()

	cases := []struct {
		name        string
		in          SendMessageInput
		wantErrPart string
	}{
		{
			name:        "zero chat_id",
			in:          SendMessageInput{Message: "hi"},
			wantErrPart: "chat_id is required",
		},
		{
			name:        "empty message",
			in:          SendMessageInput{ChatID: 1},
			wantErrPart: "message is required",
		},
		{
			name:        "message too long",
			in:          SendMessageInput{ChatID: 1, Message: strings.Repeat("x", telegramMaxMessageLength+1)},
			wantErrPart: "too long",
		},
		{
			name:        "invalid mode",
			in:          SendMessageInput{ChatID: 1, Message: "hi", Mode: "broadcast"},
			wantErrPart: "invalid mode",
		},
		{
			name:        "schedule mode without schedule_at",
			in:          SendMessageInput{ChatID: 1, Message: "hi", Mode: "schedule"},
			wantErrPart: "schedule_at is required",
		},
		{
			name:        "schedule_at without schedule mode",
			in:          SendMessageInput{ChatID: 1, Message: "hi", ScheduleAt: "2099-01-01T00:00:00Z"},
			wantErrPart: "schedule_at is only valid",
		},
		{
			name:        "invalid schedule_at format",
			in:          SendMessageInput{ChatID: 1, Message: "hi", Mode: "schedule", ScheduleAt: "not-a-date"},
			wantErrPart: "invalid schedule_at",
		},
		{
			name:        "schedule_at in the past",
			in:          SendMessageInput{ChatID: 1, Message: "hi", Mode: "schedule", ScheduleAt: "2020-01-01T00:00:00Z"},
			wantErrPart: "must be in the future",
		},
		{
			name:        "reply to scheduled handle rejected",
			in:          SendMessageInput{ChatID: 1, Message: "hi", ReplyToMessageID: "s:42"},
			wantErrPart: "cannot reply to a scheduled message",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRes, _, err := h.handle(ctx, nil, tc.in)
			require.NoError(t, err)
			require.NotNil(t, errRes)
			require.True(t, errRes.IsError)
			assert.Contains(t, toolResultText(errRes), tc.wantErrPart)
		})
	}
}

func TestChatsSearchHandlerWhitespaceValidation(t *testing.T) {
	h := NewChatsSearchHandler(nil, nil)
	ctx := context.Background()

	errRes, out, err := h.handle(ctx, nil, SearchChatsInput{Query: "   "})
	require.NoError(t, err)
	require.Nil(t, out)
	require.NotNil(t, errRes)
	require.True(t, errRes.IsError)
	assert.Contains(t, toolResultText(errRes), "query parameter is required")
}

func TestResolveUsernameHandlerWhitespaceValidation(t *testing.T) {
	h := NewUsernameResolveHandler(nil)
	ctx := context.Background()

	errRes, out, err := h.handle(ctx, nil, ResolveUsernameInput{Username: "   "})
	require.NoError(t, err)
	require.Nil(t, out)
	require.NotNil(t, errRes)
	require.True(t, errRes.IsError)
	assert.Contains(t, toolResultText(errRes), "username is required")
}

// TestMessageEditHandlerValidation covers the input-validation layer of
// MessageEditHandler.handle. Client is nil — safe because every test case
// returns before any Telegram API call is made.
func TestMessageEditHandlerValidation(t *testing.T) {
	h := &MessageEditHandler{}
	ctx := context.Background()

	cases := []struct {
		name        string
		in          EditMessageInput
		wantErrPart string
	}{
		{
			name:        "zero chat_id",
			in:          EditMessageInput{MessageID: "42", NewText: "hi"},
			wantErrPart: "chat_id is required",
		},
		{
			name:        "invalid message_id",
			in:          EditMessageInput{ChatID: 1, MessageID: "abc", NewText: "hi"},
			wantErrPart: "invalid message_id",
		},
		{
			name:        "empty new_text",
			in:          EditMessageInput{ChatID: 1, MessageID: "42"},
			wantErrPart: "new_text is required",
		},
		{
			name:        "scheduled handle requires schedule_at",
			in:          EditMessageInput{ChatID: 1, MessageID: "s:42", NewText: "hi"},
			wantErrPart: "schedule_at is required",
		},
		{
			name:        "schedule_at invalid format for scheduled handle",
			in:          EditMessageInput{ChatID: 1, MessageID: "s:42", NewText: "hi", ScheduleAt: "not-a-date"},
			wantErrPart: "invalid schedule_at",
		},
		{
			name:        "schedule_at in the past for scheduled handle",
			in:          EditMessageInput{ChatID: 1, MessageID: "s:42", NewText: "hi", ScheduleAt: "2020-01-01T00:00:00Z"},
			wantErrPart: "must be in the future",
		},
		{
			name:        "schedule_at provided for regular handle",
			in:          EditMessageInput{ChatID: 1, MessageID: "42", NewText: "hi", ScheduleAt: "2099-01-01T00:00:00Z"},
			wantErrPart: "schedule_at is only valid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRes, _, err := h.handle(ctx, nil, tc.in)
			require.NoError(t, err)
			require.NotNil(t, errRes)
			require.True(t, errRes.IsError)
			assert.Contains(t, toolResultText(errRes), tc.wantErrPart)
		})
	}
}

// TestMessageDeleteHandlerValidation covers the input-validation layer of
// MessageDeleteHandler.handle. Client is nil — safe because every test case
// returns before any Telegram API call.
func TestMessageDeleteHandlerValidation(t *testing.T) {
	h := &MessageDeleteHandler{}
	ctx := context.Background()

	cases := []struct {
		name        string
		in          DeleteMessageInput
		wantErrPart string
	}{
		{
			name:        "zero chat_id",
			in:          DeleteMessageInput{MessageID: "42"},
			wantErrPart: "chat_id is required",
		},
		{
			name:        "invalid message_id",
			in:          DeleteMessageInput{ChatID: 1, MessageID: "abc"},
			wantErrPart: "invalid message_id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRes, _, err := h.handle(ctx, nil, tc.in)
			require.NoError(t, err)
			require.NotNil(t, errRes)
			require.True(t, errRes.IsError)
			assert.Contains(t, toolResultText(errRes), tc.wantErrPart)
		})
	}
}

// TestSetReactionHandlerValidation covers the input-validation layer of
// MessageReactionHandler.handle. Client is nil — safe because every test case
// returns before any Telegram API call.
func TestSetReactionHandlerValidation(t *testing.T) {
	h := &MessageReactionHandler{}
	ctx := context.Background()

	cases := []struct {
		name        string
		in          SetReactionInput
		wantErrPart string
	}{
		{
			name:        "zero chat_id",
			in:          SetReactionInput{MessageID: "42", Emojis: []string{"👍"}},
			wantErrPart: "chat_id is required",
		},
		{
			name:        "invalid message_id",
			in:          SetReactionInput{ChatID: 1, MessageID: "abc"},
			wantErrPart: "invalid message_id",
		},
		{
			name:        "scheduled handle rejected",
			in:          SetReactionInput{ChatID: 1, MessageID: "s:42", Emojis: []string{"👍"}},
			wantErrPart: "cannot react to a scheduled message",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRes, _, err := h.handle(ctx, nil, tc.in)
			require.NoError(t, err)
			require.NotNil(t, errRes)
			require.True(t, errRes.IsError)
			assert.Contains(t, toolResultText(errRes), tc.wantErrPart)
		})
	}
}

// TestMessageForwardHandlerValidation covers the input-validation layer of
// MessageForwardHandler.handle. Client is nil — safe because every test case
// returns before any Telegram API call.
func TestMessageForwardHandlerValidation(t *testing.T) {
	h := &MessageForwardHandler{}
	ctx := context.Background()

	cases := []struct {
		name        string
		in          ForwardMessageInput
		wantErrPart string
	}{
		{
			name:        "zero from_chat_id",
			in:          ForwardMessageInput{MessageID: "42", ToChatID: 2},
			wantErrPart: "from_chat_id is required",
		},
		{
			name:        "invalid message_id",
			in:          ForwardMessageInput{FromChatID: 1, MessageID: "abc", ToChatID: 2},
			wantErrPart: "invalid message_id",
		},
		{
			name:        "scheduled handle rejected",
			in:          ForwardMessageInput{FromChatID: 1, MessageID: "s:42", ToChatID: 2},
			wantErrPart: "scheduled",
		},
		{
			name:        "zero to_chat_id",
			in:          ForwardMessageInput{FromChatID: 1, MessageID: "42"},
			wantErrPart: "to_chat_id is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRes, _, err := h.handle(ctx, nil, tc.in)
			require.NoError(t, err)
			require.NotNil(t, errRes)
			require.True(t, errRes.IsError)
			assert.Contains(t, toolResultText(errRes), tc.wantErrPart)
		})
	}
}

// TestJoinChatHandlerValidation covers the input-validation layer of
// JoinChatHandler.handle. Client is nil — safe because the empty-chat case
// returns before any Telegram API call.
func TestJoinChatHandlerValidation(t *testing.T) {
	h := &JoinChatHandler{}
	ctx := context.Background()

	errRes, _, err := h.handle(ctx, nil, JoinChatInput{Chat: "  "})
	require.NoError(t, err)
	require.NotNil(t, errRes)
	require.True(t, errRes.IsError)
	assert.Contains(t, toolResultText(errRes), "chat is required")
}

// TestLeaveChatHandlerValidation covers the input-validation layer of
// LeaveChatHandler.handle. The empty-chat case returns before the destructive
// confirmation, so a nil client/session is safe.
func TestLeaveChatHandlerValidation(t *testing.T) {
	h := &LeaveChatHandler{}
	ctx := context.Background()

	errRes, _, err := h.handle(ctx, nil, LeaveChatInput{Chat: ""})
	require.NoError(t, err)
	require.NotNil(t, errRes)
	require.True(t, errRes.IsError)
	assert.Contains(t, toolResultText(errRes), "chat is required")
}

// TestMessageContextGetHandlerValidation covers the validation paths of
// MessageContextGetHandler.handle. Provider is nil — safe because every case
// returns before a provider call.
func TestMessageContextGetHandlerValidation(t *testing.T) {
	h := &MessageContextGetHandler{}
	ctx := context.Background()

	cases := []struct {
		name        string
		in          GetMessageContextInput
		wantErrPart string
	}{
		{
			name:        "zero chat_id",
			in:          GetMessageContextInput{MessageID: "42"},
			wantErrPart: "chat_id is required",
		},
		{
			name:        "empty message_id",
			in:          GetMessageContextInput{ChatID: 1},
			wantErrPart: "message_id is required",
		},
		{
			name:        "invalid message_id",
			in:          GetMessageContextInput{ChatID: 1, MessageID: "abc"},
			wantErrPart: "invalid message_id",
		},
		{
			name:        "scheduled handle rejected",
			in:          GetMessageContextInput{ChatID: 1, MessageID: "s:42"},
			wantErrPart: "cannot get context around a scheduled message",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRes, _, err := h.handle(ctx, nil, tc.in)
			require.NoError(t, err)
			require.NotNil(t, errRes)
			require.True(t, errRes.IsError)
			assert.Contains(t, toolResultText(errRes), tc.wantErrPart)
		})
	}
}

// TestGetMessagesDateInversionValidation checks that the handler rejects a
// request where from_date is after to_date before making any Telegram API call.
func TestGetMessagesDateInversionValidation(t *testing.T) {
	h := NewMessagesGetHandler(nil)
	ctx := context.Background()

	errRes, _, err := h.handle(ctx, nil, GetMessagesInput{
		ChatID:   1,
		FromDate: "2026-04-10T23:59:59Z",
		ToDate:   "2026-04-01T00:00:00Z",
	})
	require.NoError(t, err)
	require.NotNil(t, errRes)
	require.True(t, errRes.IsError)
	assert.Contains(t, toolResultText(errRes), "after")
}

// TestSearchMessagesDateInversionValidation checks the same guard in the
// SearchMessages handler.
func TestSearchMessagesDateInversionValidation(t *testing.T) {
	h := NewMessagesSearchHandler(nil)
	ctx := context.Background()

	errRes, _, err := h.handle(ctx, nil, SearchMessagesInput{
		ChatID:   1,
		Query:    "hello",
		FromDate: "2026-04-10T23:59:59Z",
		ToDate:   "2026-04-01T00:00:00Z",
	})
	require.NoError(t, err)
	require.NotNil(t, errRes)
	require.True(t, errRes.IsError)
	assert.Contains(t, toolResultText(errRes), "after")
}

// TestSearchMessagesGlobalDateInversionValidation checks the same guard in the
// SearchMessagesGlobal handler — the three date-inversion guards should all be
// tested so a future copy/paste omission is immediately visible.
func TestSearchMessagesGlobalDateInversionValidation(t *testing.T) {
	h := NewMessagesSearchGlobalHandler(nil)
	ctx := context.Background()

	errRes, _, err := h.handle(ctx, nil, SearchMessagesGlobalInput{
		Query:    "hello",
		FromDate: "2026-04-10T23:59:59Z",
		ToDate:   "2026-04-01T00:00:00Z",
	})
	require.NoError(t, err)
	require.NotNil(t, errRes)
	require.True(t, errRes.IsError)
	assert.Contains(t, toolResultText(errRes), "after")
}

// TestMarkAsReadHandlerValidation covers the input-validation layer of
// MessageReadHandler.handle. Client is nil — safe because every test case
// returns before any Telegram API call.
func TestMarkAsReadHandlerValidation(t *testing.T) {
	h := NewMessageReadHandler(nil)
	ctx := context.Background()

	cases := []struct {
		name        string
		in          MarkAsReadInput
		wantErrPart string
	}{
		{
			name:        "empty chat_ids",
			in:          MarkAsReadInput{},
			wantErrPart: "chat_ids is required",
		},
		{
			name:        "too many chat_ids",
			in:          MarkAsReadInput{ChatIDs: make([]int64, 101)},
			wantErrPart: "more than 100",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRes, _, err := h.handle(ctx, nil, tc.in)
			require.NoError(t, err)
			require.NotNil(t, errRes)
			require.True(t, errRes.IsError)
			assert.Contains(t, toolResultText(errRes), tc.wantErrPart)
		})
	}
}

// TestBackupMessagesDateInversionValidation checks that BackupMessages rejects
// an inverted date window before making any Telegram API call. to_date is
// exclusive, so an equal or earlier to_date yields an empty window.
func TestBackupMessagesDateInversionValidation(t *testing.T) {
	h := NewMessageBackupHandler(nil, nil, nil)
	ctx := context.Background()

	errRes, _, err := h.handle(ctx, nil, BackupMessagesInput{
		ChatID:   1,
		FromDate: "2026-04-11",
		ToDate:   "2026-04-09",
	})
	require.NoError(t, err)
	require.NotNil(t, errRes)
	require.True(t, errRes.IsError)
	assert.Contains(t, toolResultText(errRes), "empty")
}
