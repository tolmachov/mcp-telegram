package tgdata

import "time"

// UserInfo represents information about a Telegram user
type UserInfo struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
	Phone     string `json:"phone,omitempty"`
	Bio       string `json:"bio,omitempty"`
	Premium   bool   `json:"premium,omitempty"`
}

// ChatInfo represents basic information about a chat
type ChatInfo struct {
	ID           int64  `json:"id"`
	Type         string `json:"type"`
	Name         string `json:"name"`
	Username     string `json:"username,omitempty"`
	UnreadCount  int    `json:"unread_count"`
	MentionCount int    `json:"mention_count"`
	Muted        bool   `json:"muted"`
	Pinned       bool   `json:"pinned"`
	Archived     bool   `json:"archived"`
	Deleted      bool   `json:"deleted"`
}

// ChatFullInfo represents detailed information about a chat
type ChatFullInfo struct {
	ChatInfo
	Description  string `json:"description,omitempty"`
	MembersCount int    `json:"members_count,omitempty"`
}

// ChatsList represents a list of chats
type ChatsList struct {
	Chats []ChatInfo `json:"chats"`
	Count int        `json:"count"`
}

// MessageInfo represents a message in a chat
type MessageInfo struct {
	ID        int        `json:"id"`
	Date      time.Time  `json:"date"`
	SenderID  int64      `json:"sender_id,omitempty"`
	Text      string     `json:"text"`
	ReplyToID int        `json:"reply_to_id,omitempty"`
	Media     *MediaInfo `json:"media,omitempty"`
	Entities  []string   `json:"entities,omitempty"`
}

// MediaInfo represents media attached to a message
type MediaInfo struct {
	Type string `json:"type"`
}

// MessagesList represents a list of messages
type MessagesList struct {
	ChatID   int64         `json:"chat_id"`
	Messages []MessageInfo `json:"messages"`
	Count    int           `json:"count"`
	HasMore  bool          `json:"has_more"`
	NextID   int           `json:"next_id,omitempty"`
}
