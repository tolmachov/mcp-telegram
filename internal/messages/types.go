package messages

import (
	"time"

	"github.com/gotd/td/tg"
)

// Message represents a Telegram message with parsed metadata.
type Message struct {
	ID         int         `json:"id"`
	Date       time.Time   `json:"date"`
	SenderID   int64       `json:"sender_id,omitempty"`
	SenderName string      `json:"sender_name,omitempty"`
	Text       string      `json:"text"`
	ReplyToID  int         `json:"reply_to_id,omitempty"`
	Media      *MediaInfo  `json:"media,omitempty"`
	Entities   []string    `json:"entities,omitempty"`
	Raw        *tg.Message `json:"-"` // Original message for advanced use cases
}

// MediaInfo represents media attached to a message.
type MediaInfo struct {
	Type string `json:"type"`
}

// FetchResult contains messages and metadata from a fetch operation.
type FetchResult struct {
	ChatID   int64            `json:"chat_id"`
	Messages []Message        `json:"messages"`
	Users    map[int64]string `json:"-"` // UserID -> display name
	Chats    map[int64]string `json:"-"` // ChatID/ChannelID -> title
	Count    int              `json:"count"`
	HasMore  bool             `json:"has_more"`
	NextID   int              `json:"next_id,omitempty"`
	Total    int              `json:"-"` // Total messages in the chat (from API)
}

// FetchOptions configures message fetching.
type FetchOptions struct {
	Limit      int
	OffsetID   int
	OffsetDate time.Time
	MinDate    time.Time // Filter: only messages after this date
	MaxDate    time.Time // Filter: only messages before this date
	UnreadOnly bool
	MaxCount   int // Stop after collecting this many messages (0 = no limit)
}

// BatchCallback is called after each batch is fetched.
// Parameters: batch number, messages collected so far, earliest message time in batch.
type BatchCallback func(batch int, collected int, earliestTime time.Time)

// DefaultFetchOptions returns sensible defaults for message fetching.
func DefaultFetchOptions() FetchOptions {
	return FetchOptions{
		Limit: 50,
	}
}
