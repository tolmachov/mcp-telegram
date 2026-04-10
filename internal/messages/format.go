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
	sb.Grow(len(messages) * 256)

	for _, msg := range messages {
		body := backupMessageBody(msg)
		if body == "" {
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
		sb.WriteString(body)
		sb.WriteByte('\n')
	}

	if sb.Len() > 0 {
		sb.WriteString("-----")
	}

	return sb.String()
}

// backupMessageBody returns the textual payload to persist in a backup. Text
// messages are stored verbatim. Media-only messages are preserved as compact
// placeholders so the export does not silently drop them.
func backupMessageBody(msg Message) string {
	if msg.Text != "" {
		return msg.Text
	}
	if msg.Media == nil {
		return ""
	}

	var parts []string
	if msg.Media.Type != "" {
		parts = append(parts, msg.Media.Type)
	}
	if msg.Media.FileName != "" {
		parts = append(parts, "file="+msg.Media.FileName)
	}
	if msg.Media.URL != "" {
		parts = append(parts, "url="+msg.Media.URL)
	}
	if msg.Media.ResourceURI != "" {
		parts = append(parts, "resource="+msg.Media.ResourceURI)
	}
	if msg.Media.Width > 0 && msg.Media.Height > 0 {
		parts = append(parts, fmt.Sprintf("size=%dx%d", msg.Media.Width, msg.Media.Height))
	}
	if len(parts) == 0 {
		return ""
	}
	return "[media: " + strings.Join(parts, ", ") + "]"
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
