package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

func TestParseTMeLink(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantErr  bool
		username string
		channel  int64
		topic    int
		msgID    int
	}{
		{
			name:     "public https",
			input:    "https://t.me/durov/123",
			username: "durov",
			msgID:    123,
		},
		{
			name:     "public http",
			input:    "http://t.me/durov/123",
			username: "durov",
			msgID:    123,
		},
		{
			name:     "bare t.me",
			input:    "t.me/durov/123",
			username: "durov",
			msgID:    123,
		},
		{
			name:     "telegram.me host",
			input:    "https://telegram.me/durov/123",
			username: "durov",
			msgID:    123,
		},
		{
			name:     "with @ prefix",
			input:    "@t.me/durov/123",
			username: "durov",
			msgID:    123,
		},
		{
			name:     "public forum topic",
			input:    "https://t.me/groupname/42/999",
			username: "groupname",
			topic:    42,
			msgID:    999,
		},
		{
			name:    "private channel",
			input:   "https://t.me/c/1234567890/42",
			channel: 1234567890,
			msgID:   42,
		},
		{
			name:    "private channel forum topic",
			input:   "https://t.me/c/1234567890/11/42",
			channel: 1234567890,
			topic:   11,
			msgID:   42,
		},
		{
			name:     "tg:// resolve form",
			input:    "tg://resolve?domain=durov&post=123",
			username: "durov",
			msgID:    123,
		},
		{
			name:     "tg:// resolve with thread",
			input:    "tg://resolve?domain=group&post=999&thread=42",
			username: "group",
			topic:    42,
			msgID:    999,
		},
		{
			name:     "trailing slash tolerated",
			input:    "https://t.me/durov/123/",
			username: "durov",
			msgID:    123,
		},
		{
			name:     "telegram.dog host",
			input:    "https://telegram.dog/durov/123",
			username: "durov",
			msgID:    123,
		},

		// Rejections.
		{name: "empty", input: "", wantErr: true},
		{name: "joinchat invite", input: "https://t.me/joinchat/abcDEF", wantErr: true},
		{name: "plus invite", input: "https://t.me/+abcDEF", wantErr: true},
		{name: "no message id", input: "https://t.me/durov", wantErr: true},
		{name: "non-numeric id", input: "https://t.me/durov/abc", wantErr: true},
		{name: "negative id", input: "https://t.me/durov/-1", wantErr: true},
		{name: "zero id", input: "https://t.me/durov/0", wantErr: true},
		{name: "private missing id", input: "https://t.me/c/1234567890", wantErr: true},
		{name: "private non-numeric channel", input: "https://t.me/c/abc/42", wantErr: true},
		{name: "addstickers reserved", input: "https://t.me/addstickers/foo", wantErr: true},
		{name: "share reserved", input: "https://t.me/share/url", wantErr: true},
		{name: "bad host", input: "https://example.com/durov/123", wantErr: true},
		{name: "too many segments", input: "https://t.me/durov/1/2/3", wantErr: true},
		{name: "username too short", input: "https://t.me/abcd/123", wantErr: true},
		{name: "username starts with digit", input: "https://t.me/1durov/123", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTMeLink(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.username, got.Username)
			assert.Equal(t, tt.channel, got.ChannelRaw)
			assert.Equal(t, tt.topic, got.TopicID)
			assert.Equal(t, tt.msgID, got.MessageID)
		})
	}
}

// TestValidateUsernameLength verifies the 5–32 character boundary.
func TestValidateUsernameLength(t *testing.T) {
	valid32 := strings.Repeat("a", 32)
	invalid33 := strings.Repeat("a", 33)

	assert.NoError(t, validateUsername(valid32))
	assert.Error(t, validateUsername(invalid33))
}

// TestResolveMessageLinkPrivateForumTopicID verifies that handle() populates
// TopicMessageID for private-channel forum links without making an API call
// (nil client is safe for the private-channel path).
func TestResolveMessageLinkPrivateForumTopicID(t *testing.T) {
	h := NewMessageLinkResolveHandler(nil)
	ctx := context.Background()

	// https://t.me/c/1234567890/11/42 → channel=1234567890, topic=11, msg=42
	_, out, err := h.handle(ctx, nil, ResolveMessageLinkInput{
		Link: "https://t.me/c/1234567890/11/42",
	})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, FormatRegularRef(11), out.TopicMessageID, "TopicMessageID should be opaque handle for topic root")
	assert.Equal(t, 11, out.TopicID)
	assert.Equal(t, FormatRegularRef(42), out.MessageID)
	assert.Equal(t, -(tgclient.ChannelIDPrefix + int64(1234567890)), out.ChatID)
}

// TestResolveMessageLinkNoTopicIDWhenNonForum verifies that TopicMessageID is
// empty for non-forum links (no topic segment in the URL).
func TestResolveMessageLinkNoTopicIDWhenNonForum(t *testing.T) {
	h := NewMessageLinkResolveHandler(nil)
	ctx := context.Background()

	_, out, err := h.handle(ctx, nil, ResolveMessageLinkInput{
		Link: "https://t.me/c/1234567890/42",
	})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Empty(t, out.TopicMessageID)
	assert.Equal(t, 0, out.TopicID)
	assert.Equal(t, FormatRegularRef(42), out.MessageID)
}

// TestChatIDFromResolved verifies that channel IDs are converted to the
// -100-prefixed user-facing form and that Chat / User IDs pass through as-is.
func TestChatIDFromResolved(t *testing.T) {
	const rawChannelID int64 = 1234567890

	tests := []struct {
		name      string
		resolved  *tg.ContactsResolvedPeer
		wantID    int64
		wantTitle string
		wantFound bool
	}{
		{
			name: "channel gets -100 prefix",
			resolved: &tg.ContactsResolvedPeer{
				Chats: []tg.ChatClass{
					&tg.Channel{ID: rawChannelID, Title: "Test Channel"},
				},
			},
			wantID:    -(tgclient.ChannelIDPrefix + rawChannelID),
			wantTitle: "Test Channel",
			wantFound: true,
		},
		{
			name: "chat ID passes through unchanged",
			resolved: &tg.ContactsResolvedPeer{
				Chats: []tg.ChatClass{
					&tg.Chat{ID: 42, Title: "Test Chat"},
				},
			},
			wantID:    42,
			wantTitle: "Test Chat",
			wantFound: true,
		},
		{
			name: "user ID passes through unchanged",
			resolved: &tg.ContactsResolvedPeer{
				Users: []tg.UserClass{
					&tg.User{ID: 99, FirstName: "Alice", LastName: "Smith"},
				},
			},
			wantID:    99,
			wantTitle: "Alice Smith",
			wantFound: true,
		},
		{
			name:      "empty resolved returns not found",
			resolved:  &tg.ContactsResolvedPeer{},
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, title, found := chatIDFromResolved(tt.resolved)
			assert.Equal(t, tt.wantFound, found)
			if !tt.wantFound {
				return
			}
			assert.Equal(t, tt.wantID, id)
			assert.Equal(t, tt.wantTitle, title)
		})
	}
}
