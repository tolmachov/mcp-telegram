// Package presentation owns stable MCP-facing representations shared by tools
// and resources.
package presentation

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tolmachov/mcp-telegram/internal/messages"
)

const scheduledPrefix = "s:"

type MessageRef struct {
	ID        int
	Scheduled bool
}

func ParseMessageRef(s string) (MessageRef, error) {
	if s == "" {
		return MessageRef{}, fmt.Errorf("message_id is required")
	}
	if s != strings.TrimSpace(s) {
		return MessageRef{}, fmt.Errorf("invalid message_id %q: contains whitespace", s)
	}
	scheduled := false
	numPart := s
	if after, ok := strings.CutPrefix(s, scheduledPrefix); ok {
		scheduled = true
		numPart = after
		if numPart == "" {
			return MessageRef{}, fmt.Errorf("invalid message_id %q: missing ID after %q prefix", s, scheduledPrefix)
		}
	}
	if numPart != strings.TrimSpace(numPart) {
		return MessageRef{}, fmt.Errorf("invalid message_id %q: contains whitespace", s)
	}
	if len(numPart) > 1 && numPart[0] == '0' {
		return MessageRef{}, fmt.Errorf("invalid message_id %q: leading zeros not allowed", s)
	}
	id, err := strconv.Atoi(numPart)
	if err != nil {
		return MessageRef{}, fmt.Errorf("invalid message_id %q: %w", s, err)
	}
	if id <= 0 {
		return MessageRef{}, fmt.Errorf("invalid message_id %q: must be a positive integer", s)
	}
	return MessageRef{ID: id, Scheduled: scheduled}, nil
}

func (r MessageRef) Format() string {
	if r.ID <= 0 {
		panic(fmt.Sprintf("MessageRef.Format: ID must be positive, got %d", r.ID))
	}
	if r.Scheduled {
		return scheduledPrefix + strconv.Itoa(r.ID)
	}
	return strconv.Itoa(r.ID)
}

func FormatRegularRef(id int) string { return MessageRef{ID: id}.Format() }

func FormatScheduledRef(id int) string { return MessageRef{ID: id, Scheduled: true}.Format() }

// Message is the shared MCP-facing message DTO. All message IDs are opaque
// strings and therefore have identical representation in tools and resources.
type Message struct {
	ID         string                  `json:"id"`
	ReplyToID  string                  `json:"reply_to_id,omitempty"`
	Date       time.Time               `json:"date"`
	SenderID   int64                   `json:"sender_id,omitempty"`
	SenderName string                  `json:"sender_name,omitempty"`
	Text       string                  `json:"text"`
	Media      *messages.MediaInfo     `json:"media,omitempty"`
	Entities   []string                `json:"entities,omitempty"`
	Reactions  []messages.ReactionInfo `json:"reactions,omitempty"`
	Replies    *messages.RepliesInfo   `json:"replies,omitempty"`
}

func FromMessage(m messages.Message, scheduled bool) Message {
	id := FormatRegularRef(m.ID)
	if scheduled {
		id = FormatScheduledRef(m.ID)
	}
	dto := Message{
		ID: id, Date: m.Date, SenderID: m.SenderID, SenderName: m.SenderName,
		Text: m.Text, Media: m.Media, Entities: m.Entities, Reactions: m.Reactions, Replies: m.Replies,
	}
	if m.ReplyToID > 0 {
		dto.ReplyToID = FormatRegularRef(m.ReplyToID)
	}
	return dto
}
