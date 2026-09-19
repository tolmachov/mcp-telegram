package presentation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/messages"
)

func TestMessageRefParseAndFormat(t *testing.T) {
	for input, want := range map[string]MessageRef{
		"42":           {ID: 42},
		"s:42":         {ID: 42, Scheduled: true},
		"2147483647":   {ID: 2147483647},
		"s:2147483647": {ID: 2147483647, Scheduled: true},
	} {
		got, err := ParseMessageRef(input)
		require.NoError(t, err)
		assert.Equal(t, want, got)
		assert.Equal(t, input, got.Format())
	}
	for _, input := range []string{"", " 42", "42 ", "s:", "01", "s:01", "0", "-1", "nope", "2147483648", "s:2147483648", "4294967338", "s:4294967338"} {
		_, err := ParseMessageRef(input)
		assert.Error(t, err, input)
	}
	assert.Panics(t, func() { _ = (MessageRef{}).Format() })
	assert.Equal(t, "7", FormatRegularRef(7))
	assert.Equal(t, "s:8", FormatScheduledRef(8))
}

func TestFromMessageUsesUniformOpaqueHandles(t *testing.T) {
	now := time.Now()
	input := messages.Message{ID: 9, ReplyToID: 7, Date: now, SenderID: 3, SenderName: "Alice", Text: "hello"}
	regular := FromMessage(input, false)
	assert.Equal(t, "9", regular.ID)
	assert.Equal(t, "7", regular.ReplyToID)
	assert.Equal(t, now, regular.Date)
	assert.Equal(t, "hello", regular.Text)
	scheduled := FromMessage(input, true)
	assert.Equal(t, "s:9", scheduled.ID)
	assert.Equal(t, "7", scheduled.ReplyToID)

	input.ReplyToID = 0
	assert.Empty(t, FromMessage(input, false).ReplyToID)
}
