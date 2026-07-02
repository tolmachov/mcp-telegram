package tgdata

// UserInfo represents information about a Telegram user.
type UserInfo struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
	Phone     string `json:"phone,omitempty"`
	Bio       string `json:"bio,omitempty"`
	Premium   bool   `json:"premium,omitempty"`
}

// ChatType classifies a Telegram dialog.
type ChatType string

// Known chat types.
const (
	ChatTypeUser       ChatType = "user"
	ChatTypeBot        ChatType = "bot"
	ChatTypeGroup      ChatType = "group"
	ChatTypeSupergroup ChatType = "supergroup"
	ChatTypeChannel    ChatType = "channel"
)

// ChatInfo represents basic information about a chat.
type ChatInfo struct {
	ID           int64    `json:"id"`
	Type         ChatType `json:"type"`
	Name         string   `json:"name"`
	Username     string   `json:"username,omitempty"`
	UnreadCount  int      `json:"unread_count"`
	MentionCount int      `json:"mention_count"`
	Muted        bool     `json:"muted"`
	Pinned       bool     `json:"pinned"`
	Archived     bool     `json:"archived"`
}

// ChatFullInfo represents detailed information about a chat.
type ChatFullInfo struct {
	ChatInfo
	Description  string `json:"description,omitempty"`
	MembersCount int    `json:"members_count,omitempty"`
}

// ChatsList represents a list of chats.
type ChatsList struct {
	Chats []ChatInfo `json:"chats"`
	Count int        `json:"count"`
	// Truncated is true when dialog pagination stalled before the whole list
	// was fetched (a phantom-dialog anomaly that fails the cursor-advance
	// guard). The returned Chats are a prefix of the real listing, so callers
	// must not treat an absent chat as proof it doesn't exist.
	Truncated bool `json:"truncated,omitempty"`
}
