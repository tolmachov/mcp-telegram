package tools

import "fmt"

const messagePageCursorVersion = 1

const (
	cursorKindHistory = "history"
	cursorKindSearch  = "search"
	cursorKindReplies = "replies"
)

// messagePageCursor is the complete immutable query plus Telegram's raw
// continuation anchor. A continuation request therefore contains only the
// cursor: callers cannot accidentally change filters between pages.
type messagePageCursor struct {
	Version          int    `json:"v"`
	Kind             string `json:"k"`
	ChatID           int64  `json:"c"`
	OffsetID         int    `json:"o"`
	Limit            int    `json:"l"`
	FromDate         string `json:"f,omitempty"`
	ToDate           string `json:"t,omitempty"`
	UnreadOnly       bool   `json:"u,omitempty"`
	IncludeScheduled bool   `json:"s,omitempty"`
	Query            string `json:"q,omitempty"`
	FromSenderID     int64  `json:"p,omitempty"`
	MediaType        string `json:"m,omitempty"`
	TopMsgID         int    `json:"x,omitempty"`
	RootMessageID    int    `json:"r,omitempty"`
}

func (c messagePageCursor) cursorVersion() int { return c.Version }

func formatMessagePageCursor(c messagePageCursor) string {
	c.Version = messagePageCursorVersion
	return encodeCursor(c)
}

func parseMessagePageCursor(raw, kind string) (messagePageCursor, error) {
	c, err := decodeCursor[messagePageCursor](raw, messagePageCursorVersion)
	if err != nil {
		return messagePageCursor{}, err
	}
	if c.Kind != kind {
		return messagePageCursor{}, fmt.Errorf("cursor belongs to %s, not %s", c.Kind, kind)
	}
	if c.ChatID == 0 || c.OffsetID <= 0 || c.Limit <= 0 || c.Limit > 100 {
		return messagePageCursor{}, fmt.Errorf("cursor contains invalid pagination state")
	}
	return c, nil
}
