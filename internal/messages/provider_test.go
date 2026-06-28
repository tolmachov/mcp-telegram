package messages

import (
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractReactions(t *testing.T) {
	chosen := tg.ReactionCount{Reaction: &tg.ReactionEmoji{Emoticon: "👍"}, Count: 3}
	chosen.SetChosenOrder(0)

	r := tg.MessageReactions{
		Results: []tg.ReactionCount{
			chosen,
			{Reaction: &tg.ReactionCustomEmoji{DocumentID: 12345}, Count: 2},
			{Reaction: &tg.ReactionPaid{}, Count: 5},
			{Reaction: &tg.ReactionEmpty{}, Count: 1}, // skipped
		},
	}

	got := extractReactions(r)
	require.Len(t, got, 3)

	assert.Equal(t, ReactionInfo{Emoji: "👍", Count: 3, Chosen: true}, got[0])
	assert.Equal(t, ReactionInfo{CustomEmojiID: "12345", Count: 2}, got[1])
	assert.Equal(t, ReactionInfo{Paid: true, Count: 5}, got[2])
}

func TestExtractReactionsEmpty(t *testing.T) {
	assert.Nil(t, extractReactions(tg.MessageReactions{}))
}

func TestExtractSubstring(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		offset int
		length int
		want   string
	}{
		{
			name:   "ASCII text",
			input:  "Hello, World!",
			offset: 7,
			length: 5,
			want:   "World",
		},
		{
			name:   "ASCII from start",
			input:  "Hello",
			offset: 0,
			length: 5,
			want:   "Hello",
		},
		{
			name:   "Cyrillic text",
			input:  "Привет, мир!",
			offset: 8,
			length: 3,
			want:   "мир",
		},
		{
			name:   "Cyrillic from start",
			input:  "Привет",
			offset: 0,
			length: 6,
			want:   "Привет",
		},
		{
			name:   "Emoji (surrogate pair)",
			input:  "Hello 👋 World",
			offset: 6,
			length: 2, // emoji takes 2 code units
			want:   "👋",
		},
		{
			name:   "Text after emoji",
			input:  "Hello 👋 World",
			offset: 9, // 6 + 2 (emoji) + 1 (space)
			length: 5,
			want:   "World",
		},
		{
			name:   "Multiple emojis",
			input:  "🎉🎊🎁",
			offset: 2, // after first emoji
			length: 2, // second emoji
			want:   "🎊",
		},
		{
			name:   "Mixed: Cyrillic and emoji",
			input:  "Привет 👋",
			offset: 0,
			length: 6,
			want:   "Привет",
		},
		{
			name:   "Mixed: emoji in Cyrillic",
			input:  "Привет 👋 мир",
			offset: 7,
			length: 2,
			want:   "👋",
		},
		{
			name:   "URL in text",
			input:  "Check https://example.com for info",
			offset: 6,
			length: 19,
			want:   "https://example.com",
		},
		{
			name:   "URL after Cyrillic",
			input:  "Ссылка: https://example.com",
			offset: 8,
			length: 19,
			want:   "https://example.com",
		},
		{
			name:   "Empty string",
			input:  "",
			offset: 0,
			length: 1,
			want:   "",
		},
		{
			name:   "Negative offset",
			input:  "Hello",
			offset: -1,
			length: 3,
			want:   "",
		},
		{
			name:   "Zero length",
			input:  "Hello",
			offset: 0,
			length: 0,
			want:   "",
		},
		{
			name:   "Negative length",
			input:  "Hello",
			offset: 0,
			length: -1,
			want:   "",
		},
		{
			name:   "Offset beyond string",
			input:  "Hello",
			offset: 10,
			length: 1,
			want:   "",
		},
		{
			name:   "Length exceeds string",
			input:  "Hello",
			offset: 0,
			length: 100,
			want:   "",
		},
		{
			name:   "Single character",
			input:  "A",
			offset: 0,
			length: 1,
			want:   "A",
		},
		{
			name:   "Single Cyrillic character",
			input:  "Я",
			offset: 0,
			length: 1,
			want:   "Я",
		},
		{
			name:   "Single emoji",
			input:  "🔥",
			offset: 0,
			length: 2,
			want:   "🔥",
		},
		{
			name:   "Flag emoji (ZWJ sequence)",
			input:  "Hi 🇺🇸 there",
			offset: 3,
			length: 4, // flag emoji: 2 regional indicators × 2 code units each
			want:   "🇺🇸",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSubstring(tt.input, tt.offset, tt.length)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestProcessHistoryNotModified verifies that MessagesMessagesNotModified is
// treated as an empty (exhausted) page rather than an error. Callers rely on
// this to terminate FetchAll pagination without failing.
func TestProcessHistoryNotModified(t *testing.T) {
	p := NewProvider(nil)
	result, err := p.processHistory(&tg.MessagesMessagesNotModified{}, &tg.InputPeerEmpty{})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Empty(t, result.Messages)
	assert.False(t, result.HasMore)
}

// TestExtractMessagesDropsZeroID verifies that messages with ID <= 0 are
// silently dropped. Telegram guarantees positive IDs, but a zero-ID message
// would cause FormatRegularRef to panic at the tool boundary.
func TestExtractMessagesDropsZeroID(t *testing.T) {
	p := NewProvider(nil)
	msgs := []tg.MessageClass{
		&tg.Message{ID: 0, Message: "zero"},
		&tg.Message{ID: 1, Message: "valid"},
		&tg.Message{ID: -1, Message: "negative"},
	}
	got := p.extractMessages(msgs, nil, nil, &tg.InputPeerEmpty{})
	require.Len(t, got, 1)
	assert.Equal(t, 1, got[0].ID)
}

func TestHistoryOffsetDate(t *testing.T) {
	maxDate := time.Date(2026, 4, 10, 15, 0, 0, 0, time.UTC)
	offsetDate := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		opts FetchOptions
		want time.Time
	}{
		{
			name: "zero values",
			opts: FetchOptions{},
			want: time.Time{},
		},
		{
			name: "max date preserved exactly",
			opts: FetchOptions{MaxDate: maxDate},
			want: maxDate,
		},
		{
			name: "offset date takes precedence",
			opts: FetchOptions{MaxDate: maxDate, OffsetDate: offsetDate},
			want: offsetDate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := historyOffsetDate(tt.opts)
			assert.True(t, got.Equal(tt.want))
		})
	}
}
