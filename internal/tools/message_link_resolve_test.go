package tools

import "testing"

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
			name:    "trailing slash tolerated",
			input:   "https://t.me/durov/123/",
			username: "durov",
			msgID:   123,
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
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseTMeLink(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.Username != tt.username {
				t.Errorf("username = %q, want %q", got.Username, tt.username)
			}
			if got.ChannelRaw != tt.channel {
				t.Errorf("channel = %d, want %d", got.ChannelRaw, tt.channel)
			}
			if got.TopicID != tt.topic {
				t.Errorf("topic = %d, want %d", got.TopicID, tt.topic)
			}
			if got.MessageID != tt.msgID {
				t.Errorf("msgID = %d, want %d", got.MessageID, tt.msgID)
			}
		})
	}
}
