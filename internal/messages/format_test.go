package messages

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatBatchForBackupIncludesMediaOnlyMessages(t *testing.T) {
	out := FormatBatchForBackup([]Message{
		{
			ID:         1,
			Date:       time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
			SenderName: "Alice",
			Media: &MediaInfo{
				Type:        "photo",
				Width:       800,
				Height:      600,
				ResourceURI: "telegram://media/1/2/3/m?ref=abc",
			},
		},
	})

	assert.Contains(t, out, "[media: photo")
	assert.Contains(t, out, "resource=telegram://media/1/2/3/m?ref=abc")
}

func TestBackupMessageBodyTextPreferredOverMediaPlaceholder(t *testing.T) {
	body := backupMessageBody(Message{
		Text: "hello",
		Media: &MediaInfo{
			Type: "photo",
		},
	})
	assert.Equal(t, "hello", body)
}

func TestBackupMessageBodyFileNameBranch(t *testing.T) {
	body := backupMessageBody(Message{
		Media: &MediaInfo{
			Type:     "document",
			FileName: "report.pdf",
		},
	})
	assert.Contains(t, body, "file=report.pdf")
	assert.Contains(t, body, "document")
}

func TestBackupMessageBodyURLBranch(t *testing.T) {
	body := backupMessageBody(Message{
		Media: &MediaInfo{
			Type: "webpage",
			URL:  "https://example.com",
		},
	})
	assert.Contains(t, body, "url=https://example.com")
	assert.Contains(t, body, "webpage")
}

func TestBackupMessageBodyAllMediaFields(t *testing.T) {
	body := backupMessageBody(Message{
		Media: &MediaInfo{
			Type:        "photo",
			FileName:    "img.jpg",
			URL:         "https://cdn.example.com/img.jpg",
			ResourceURI: "telegram://media/1",
			Width:       1920,
			Height:      1080,
		},
	})
	assert.Contains(t, body, "file=img.jpg")
	assert.Contains(t, body, "url=https://cdn.example.com/img.jpg")
	assert.Contains(t, body, "resource=telegram://media/1")
	assert.Contains(t, body, "size=1920x1080")
}

// TestBackupMessageBodyAllZeroMedia verifies that a non-nil MediaInfo with all
// zero-value fields (default struct) returns an empty string rather than
// emitting a "[media: ]" placeholder with no components.
func TestBackupMessageBodyAllZeroMedia(t *testing.T) {
	body := backupMessageBody(Message{Media: &MediaInfo{}})
	assert.Empty(t, body)
}

func TestFormatBatchForBackupIncludesReplyTo(t *testing.T) {
	out := FormatBatchForBackup([]Message{
		{
			ID:         5,
			Date:       time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
			SenderName: "Bob",
			Text:       "reply text",
			ReplyToID:  3,
		},
	})

	assert.Contains(t, out, "[reply_to=3]")
}

func TestFormatBatchForBackupAllEmptyBodyReturnsEmpty(t *testing.T) {
	// Messages with no text and no media produce empty bodies and must be
	// skipped; when all messages are skipped the result must be "" (no orphan
	// "-----" trailer).
	out := FormatBatchForBackup([]Message{
		{ID: 1, Date: time.Now(), SenderName: "Alice", Text: "", Media: nil},
		{ID: 2, Date: time.Now(), SenderName: "Bob", Text: "", Media: nil},
	})

	assert.Empty(t, out)
}

func TestFilterTextOnly(t *testing.T) {
	msgs := []Message{
		{ID: 1, Text: "hello"},
		{ID: 2, Text: ""},
		{ID: 3, Text: "world"},
	}
	got := FilterTextOnly(msgs)
	require.Len(t, got, 2)
	assert.Equal(t, "hello", got[0].Text)
	assert.Equal(t, "world", got[1].Text)
}

func TestFilterTextOnlyAllEmpty(t *testing.T) {
	msgs := []Message{{ID: 1}, {ID: 2}}
	assert.Empty(t, FilterTextOnly(msgs))
}

func TestReverseOddLength(t *testing.T) {
	msgs := []Message{{ID: 1}, {ID: 2}, {ID: 3}}
	Reverse(msgs)
	assert.Equal(t, []Message{{ID: 3}, {ID: 2}, {ID: 1}}, msgs)
}

func TestReverseEmptyAndSingle(t *testing.T) {
	var empty []Message
	Reverse(empty)
	assert.Empty(t, empty)

	single := []Message{{ID: 99}}
	Reverse(single)
	assert.Equal(t, 99, single[0].ID)
}

func TestFormatBatchForSummaryIncludesTextMessages(t *testing.T) {
	msgs := []Message{
		{ID: 1, Date: time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC), SenderID: 42, Text: "hello"},
		{ID: 2, Date: time.Date(2026, 4, 10, 9, 1, 0, 0, time.UTC), SenderID: 99, Text: ""},     // no text — skipped
		{ID: 3, Date: time.Date(2026, 4, 10, 9, 2, 0, 0, time.UTC), SenderID: 42, Text: "world"},
	}
	out := FormatBatchForSummary(msgs)

	assert.Contains(t, out, "hello")
	assert.NotContains(t, out, "sender_id: \n")
	assert.Contains(t, out, "world")
}
