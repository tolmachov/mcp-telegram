package messages

import (
	"fmt"
	"log/slog"
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
	Type        string `json:"type"`
	URL         string `json:"url,omitempty"`          // URL for webpage media
	FileName    string `json:"file_name,omitempty"`    // Filename for documents
	Width       int    `json:"width,omitempty"`        // Width for photos/videos
	Height      int    `json:"height,omitempty"`       // Height for photos/videos
	ResourceURI string `json:"resource_uri,omitempty"` // MCP resource URI for downloading
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
}

// FetchOptions configures message fetching.
type FetchOptions struct {
	Limit    int
	OffsetID int
	// OffsetDate is set internally by FetchAll during pagination; do not set
	// directly. FetchAll seeds it from MaxDate on the first page only; after
	// that it is reset to zero and subsequent pages are driven by OffsetID.
	// When non-zero it is passed to Telegram as the native offset_date
	// (strictly-less-than) and overrides MaxDate for that batch.
	OffsetDate time.Time
	MinDate    time.Time // Filter: only messages after this date
	// MaxDate is the exclusive upper bound for Telegram's offset_date
	// (strictly-less-than). Callers that accept a date-only boundary (e.g.
	// YYYY-MM-DD) must advance by 24h to include that day's messages —
	// BackupMessages does this automatically via normalizeInclusiveUpperDate.
	// GetMessages / SearchMessages accept a full RFC3339 timestamp, so
	// pass midnight of the next day if the intent is to include a whole day.
	MaxDate time.Time
	UnreadOnly bool
	MaxCount   int // Stop after collecting this many messages. 0 means no limit.
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

// SearchOptions configures a substring search inside one chat.
//
// The zero value (apart from Query) is meaningful: an empty Filter means
// InputMessagesFilterEmpty (plain text search across all message types),
// FromSenderID 0 means "any sender", TopMsgID 0 means "whole chat".
type SearchOptions struct {
	Query        string
	Limit        int
	OffsetID     int
	MinDate      time.Time
	MaxDate      time.Time // Exclusive upper bound mapped to messages.search's max_date parameter (strictly-less-than).
	FromSenderID int64     // 0 = any sender
	TopMsgID     int                    // 0 = whole chat (not a forum thread)
	Filter       tg.MessagesFilterClass // nil → InputMessagesFilterEmpty
}

// GlobalSearchOptions configures a cross-chat substring search.
type GlobalSearchOptions struct {
	Query   string
	Limit   int
	MinDate time.Time
	MaxDate time.Time           // Exclusive upper bound mapped to messages.searchGlobal's max_date parameter (strictly-less-than).
	Cursor  *GlobalSearchCursor // nil = first page
}

// PeerKind classifies a Telegram peer in a GlobalSearchCursor.
type PeerKind string

// Known peer kinds.
const (
	PeerKindUser    PeerKind = "user"
	PeerKindChat    PeerKind = "chat"
	PeerKindChannel PeerKind = "channel"
)

// GlobalSearchCursor is the opaque pagination state for SearchGlobal.
// Mirrors Telegram's (offset_rate, offset_peer, offset_id) tuple so the
// next call resumes from exactly where the prior one stopped. The full
// InputPeer (kind + id + access hash) is carried inline so the continuation
// call does not need a separate peer-resolution round-trip.
//
// Construct via NewGlobalSearchCursor so the cursor's invariants (known
// peer kind, non-zero ids, access_hash present for user/channel) are
// validated at the single producer site inside the provider; the wire
// parser enforces the same invariants on inbound cursors.
type GlobalSearchCursor struct {
	Rate       int
	PeerKind   PeerKind // "user" | "chat" | "channel"
	PeerID     int64
	AccessHash int64 // required for user/channel; 0 for basic chat
	MsgID      int
}

// NewGlobalSearchCursor builds a validated cursor. It returns an error if
// any invariant is violated so the provider fails loudly in tests instead
// of emitting a cursor that the wire parser would later reject.
func NewGlobalSearchCursor(rate int, peerKind PeerKind, peerID, accessHash int64, msgID int) (*GlobalSearchCursor, error) {
	switch peerKind {
	case PeerKindUser, PeerKindChannel:
		if accessHash == 0 {
			return nil, fmt.Errorf("cursor for peer kind %q requires access_hash", peerKind)
		}
	case PeerKindChat:
		// basic chats have no access_hash; nothing to check
	default:
		return nil, fmt.Errorf("cursor has unknown peer kind %q; expected user|chat|channel", peerKind)
	}
	if peerID == 0 {
		return nil, fmt.Errorf("cursor is missing peer id")
	}
	if msgID <= 0 {
		return nil, fmt.Errorf("cursor has non-positive message id %d", msgID)
	}
	return &GlobalSearchCursor{
		Rate:       rate,
		PeerKind:   peerKind,
		PeerID:     peerID,
		AccessHash: accessHash,
		MsgID:      msgID,
	}, nil
}

// InputPeer reconstructs the Telegram InputPeerClass from the cursor so
// the next messages.searchGlobal call can pass offset_peer exactly as the
// prior response's last message. A nil receiver returns InputPeerEmpty,
// which Telegram accepts at the start of a search. An unknown kind on a
// non-nil cursor would indicate a constructor bug — NewGlobalSearchCursor
// and the wire parser both reject unknown kinds — so this function treats
// any unknown kind as a programmer error by returning InputPeerEmpty,
// which will cause Telegram to restart from the top rather than silently
// skip pages.
func (c *GlobalSearchCursor) InputPeer() tg.InputPeerClass {
	if c == nil {
		return &tg.InputPeerEmpty{}
	}
	switch c.PeerKind {
	case PeerKindUser:
		return &tg.InputPeerUser{UserID: c.PeerID, AccessHash: c.AccessHash}
	case PeerKindChat:
		return &tg.InputPeerChat{ChatID: c.PeerID}
	case PeerKindChannel:
		return &tg.InputPeerChannel{ChannelID: c.PeerID, AccessHash: c.AccessHash}
	default:
		slog.Error("GlobalSearchCursor.InputPeer: unknown peer kind; restarting pagination from beginning", "peer_kind", c.PeerKind)
		return &tg.InputPeerEmpty{}
	}
}

// GlobalSearchResult is the provider-level result of a cross-chat search.
// Unlike FetchResult each message carries its own chat attribution, since
// results span many chats.
//
// SkippedCount reports how many items from the raw page were dropped because
// they were not regular messages (MessageService, MessageEmpty, or an
// unknown PeerClass that the client cannot attribute to a chat). It's
// surfaced so the tool layer can include a hint in the response when a
// page of, say, 50 service messages would otherwise look like "zero
// matches" to the LLM.
type GlobalSearchResult struct {
	Messages     []GlobalMessage
	NextCursor   *GlobalSearchCursor // nil when there are no more pages
	HasMore      bool
	Count        int
	SkippedCount int
}

// GlobalMessage is a Message with explicit chat attribution.
// ChatID is in user-facing format: positive for users and basic chats,
// negative -100-prefixed for channels/supergroups — callers can feed it
// directly into GetMessages, SearchMessages, etc.
type GlobalMessage struct {
	Message
	ChatID    int64
	ChatTitle string
}
