package messages

import (
	"fmt"
	"strconv"
	"strings"
)

// DateFormat is the default timestamp format for messages.
const DateFormat = "2006-01-02 15:04:05"

// ShortDateFormat is a shorter timestamp format.
const ShortDateFormat = "2006-01-02 15:04"

// FormatForSummary formats a message for LLM summarization.
// Format: [timestamp] sender_id: text
func FormatForSummary(msg Message) string {
	return fmt.Sprintf("[%s] %d: %s",
		msg.Date.Format(ShortDateFormat),
		msg.SenderID,
		msg.Text,
	)
}

// FormatBatchForBackup formats a batch of messages for a backup file.
// Format: -----\n[timestamp] [sender_name] [id=N] [reply_to=N]\n<text>\n-----
func FormatBatchForBackup(messages []Message) string {
	if len(messages) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.Grow(len(messages) * 1 << 8) // Preallocate approx 256 bytes per message

	for _, msg := range messages {
		if msg.Text == "" {
			continue
		}

		sb.WriteString("-----\n[")
		sb.WriteString(msg.Date.Format(DateFormat))
		sb.WriteString("] [")
		sb.WriteString(msg.SenderName)
		sb.WriteString("] [id=")
		sb.WriteString(strconv.Itoa(msg.ID))
		sb.WriteByte(']')

		if msg.ReplyToID != 0 {
			sb.WriteString(" [reply_to=")
			sb.WriteString(strconv.Itoa(msg.ReplyToID))
			sb.WriteByte(']')
		}

		sb.WriteByte('\n')
		sb.WriteString(msg.Text)
		sb.WriteByte('\n')
	}

	if sb.Len() > 0 {
		sb.WriteString("-----")
	}

	return sb.String()
}

// FormatBatchForSummary formats a batch of messages for summarization.
func FormatBatchForSummary(messages []Message) string {
	var sb strings.Builder
	for _, msg := range messages {
		if msg.Text == "" {
			continue
		}
		sb.WriteString(FormatForSummary(msg))
		sb.WriteString("\n")
	}
	return sb.String()
}

// FilterTextOnly returns only messages with non-empty text.
func FilterTextOnly(messages []Message) []Message {
	result := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Text != "" {
			result = append(result, msg)
		}
	}
	return result
}

// Reverse reverses a slice of messages in place.
func Reverse(messages []Message) {
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
}
