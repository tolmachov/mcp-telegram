package tools

import (
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tgdata"
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
		{"negative id stays numeric", "-1001234567890", chatRefID, "-1001234567890"},
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

// TestLeaveChatRejectsInvite verifies the documented contract that LeaveChat
// refuses invite links before any network call. A nil client is safe because
// the invite branch returns before any RPC.
func TestLeaveChatRejectsInvite(t *testing.T) {
	h := &LeaveChatHandler{}
	ctx := context.Background()

	for _, ref := range []string{"https://t.me/+AbCdEf", "tg://join?invite=AbCdEf"} {
		t.Run(ref, func(t *testing.T) {
			errRes, out, err := h.handle(ctx, &mcp.CallToolRequest{}, LeaveChatInput{Chat: ref, Confirm: true})
			require.NoError(t, err)
			require.Nil(t, out)
			require.NotNil(t, errRes)
			require.True(t, errRes.IsError)
			assert.Contains(t, toolResultText(errRes), "does not accept invite links")
		})
	}
}

// leaveChatInvoker resolves a public @username to a channel and counts how many
// times channels.leaveChannel was actually invoked, so a test can prove whether
// LeaveChat reached the API or bailed out at the confirmation gate.
type leaveChatInvoker struct {
	channelID  int64
	accessHash int64
	megagroup  bool
	leaveCalls int
}

func (f *leaveChatInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch input.(type) {
	case *tg.ContactsResolveUsernameRequest:
		resolved := output.(*tg.ContactsResolvedPeer)
		resolved.Peer = &tg.PeerChannel{ChannelID: f.channelID}
		resolved.Chats = []tg.ChatClass{&tg.Channel{
			ID:         f.channelID,
			AccessHash: f.accessHash,
			Megagroup:  f.megagroup,
			Title:      "Test Channel",
		}}
		return nil
	case *tg.ChannelsLeaveChannelRequest:
		f.leaveCalls++
		output.(*tg.UpdatesBox).Updates = &tg.Updates{}
		return nil
	default:
		return fmt.Errorf("leaveChatInvoker: unexpected request %T", input)
	}
}

// TestLeaveChatConfirmBypassesElicitation verifies the fail-closed gate:
//   - Confirm=true reaches channels.leaveChannel.
//   - Confirm=false bails out at the confirmation gate and never
//     hits the API, so the account is not left behind the user's back.
func TestLeaveChatConfirmBypassesElicitation(t *testing.T) {
	newHandler := func() (*LeaveChatHandler, *leaveChatInvoker) {
		inv := &leaveChatInvoker{channelID: 555, accessHash: 999}
		return NewLeaveChatHandler(tgclient.NewResolver(tg.NewClient(inv))), inv
	}

	t.Run("confirm true leaves without elicitation", func(t *testing.T) {
		h, inv := newHandler()
		errRes, out, err := h.handle(context.Background(), &mcp.CallToolRequest{}, LeaveChatInput{
			Chat:    "@testchan",
			Confirm: true,
		})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, statusLeft, out.Status)
		assert.Equal(t, tgdata.ChatTypeChannel, out.Kind)
		assert.Equal(t, int64(555), out.ChatID)
		assert.Equal(t, 1, inv.leaveCalls, "confirm=true must reach channels.leaveChannel")
	})

	t.Run("a supergroup reports its own kind", func(t *testing.T) {
		inv := &leaveChatInvoker{channelID: 556, accessHash: 998, megagroup: true}
		h := NewLeaveChatHandler(tgclient.NewResolver(tg.NewClient(inv)))
		errRes, out, err := h.handle(context.Background(), &mcp.CallToolRequest{}, LeaveChatInput{
			Chat:    "@testgroup",
			Confirm: true,
		})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, tgdata.ChatTypeSupergroup, out.Kind, "LeaveChat must classify like JoinChat and ResolveUsername")
	})

	t.Run("confirm false without session cancels before the API", func(t *testing.T) {
		h, inv := newHandler()
		errRes, out, err := h.handle(context.Background(), &mcp.CallToolRequest{}, LeaveChatInput{
			Chat:    "@testchan",
			Confirm: false,
		})
		require.NoError(t, err)
		require.Nil(t, out)
		require.NotNil(t, errRes)
		assert.True(t, errRes.IsError)
		assert.Equal(t, 0, inv.leaveCalls, "an unconfirmed leave must never hit the API")
	})
}
