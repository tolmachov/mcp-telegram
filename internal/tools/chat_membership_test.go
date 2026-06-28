package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClassifyChatRef verifies the local (no-network) classification of a chat
// reference into an invite hash, a public username, or a numeric ID.
func TestClassifyChatRef(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantKind  string
		wantValue string
	}{
		{"empty", "", "", ""},
		{"username with at", "@durov", chatRefUsername, "durov"},
		{"username bare", "durov", chatRefUsername, "durov"},
		{"username with surrounding space", "  @durov  ", chatRefUsername, "durov"},
		{"numeric id", "123456", chatRefID, "123456"},
		{"negative bot-api id", "-1001234567890", chatRefID, "-1001234567890"},
		{"https invite plus", "https://t.me/+AbCdEf", chatRefInvite, "AbCdEf"},
		{"bare host invite plus", "t.me/+AbCdEf", chatRefInvite, "AbCdEf"},
		{"joinchat with scheme", "https://t.me/joinchat/AbCdEf", chatRefInvite, "AbCdEf"},
		{"joinchat no scheme", "t.me/joinchat/AbCdEf", chatRefInvite, "AbCdEf"},
		{"telegram.me joinchat", "telegram.me/joinchat/AbCdEf", chatRefInvite, "AbCdEf"},
		{"telegram.dog invite plus", "telegram.dog/+AbCdEf", chatRefInvite, "AbCdEf"},
		{"tg join scheme", "tg://join?invite=AbCdEf", chatRefInvite, "AbCdEf"},
		{"bare plus hash", "+AbCdEf", chatRefInvite, "AbCdEf"},
		{"public link with scheme", "https://t.me/durov", chatRefUsername, "durov"},
		{"public link no scheme", "t.me/durov", chatRefUsername, "durov"},
		// Fall-through cases: unrecognised links are treated as bare usernames so
		// the downstream resolver produces a clear "not found" rather than a panic.
		{"tg join without invite param", "tg://join", chatRefUsername, "tg://join"},
		{"joinchat without hash", "t.me/joinchat", chatRefUsername, "t.me/joinchat"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, value := classifyChatRef(tc.in)
			assert.Equal(t, tc.wantKind, kind)
			assert.Equal(t, tc.wantValue, value)
		})
	}
}

// TestChatRefFromURL covers the URL parser directly, including the false-return
// branches that TestClassifyChatRef can only reach indirectly.
func TestChatRefFromURL(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantKind  string
		wantValue string
		wantOK    bool
	}{
		{"invite plus", "https://t.me/+AbCdEf", chatRefInvite, "AbCdEf", true},
		{"invite joinchat", "t.me/joinchat/AbCdEf", chatRefInvite, "AbCdEf", true},
		{"public username", "https://t.me/durov", chatRefUsername, "durov", true},
		{"non-telegram host", "https://example.com/+AbCdEf", "", "", false},
		{"empty path", "https://t.me/", "", "", false},
		{"joinchat without hash", "t.me/joinchat", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, value, ok := chatRefFromURL(tc.in)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantKind, kind)
			assert.Equal(t, tc.wantValue, value)
		})
	}
}

// TestLeaveChatResolvePeerRejectsInvite verifies the documented contract that
// LeaveChat refuses invite links before any network call. resolvePeer is called
// directly because handle() reaches confirmDestructive first, which can't run
// against a nil session. A nil client is safe: the invite branch returns early.
func TestLeaveChatResolvePeerRejectsInvite(t *testing.T) {
	h := &LeaveChatHandler{}
	ctx := context.Background()

	for _, ref := range []string{"https://t.me/+AbCdEf", "tg://join?invite=AbCdEf"} {
		t.Run(ref, func(t *testing.T) {
			peer, errRes := h.resolvePeer(ctx, ref)
			require.Nil(t, peer)
			require.NotNil(t, errRes)
			require.True(t, errRes.IsError)
			assert.Contains(t, toolResultText(errRes), "does not accept invite links")
		})
	}
}
