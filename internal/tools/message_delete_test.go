package tools

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

const (
	testBasicChatID = 5488600533
	testChannelID   = 777
)

// deleteInvoker models one chat's message store the way Telegram treats it:
// getMessages returns messageEmpty for unknown IDs, and a revoke of a message
// the account may not delete for everyone is silently skipped (skipRevoke) —
// the case the handler must report as forbidden, not deleted.
type deleteInvoker struct {
	channel    bool
	admin      bool
	skipRevoke bool  // delete call succeeds but removes nothing
	deleteErr  error // delete call fails with this Telegram error
	rereadErr  error // reads after a delete call fail with this error
	msgs       map[int]tg.PeerClass
	out        map[int]bool
	scheduled  map[int]bool
	deleted    [][]int // IDs sent to each delete call
}

func newBasicGroupInvoker() *deleteInvoker {
	return &deleteInvoker{
		msgs: map[int]tg.PeerClass{},
		out:  map[int]bool{},
	}
}

// add stores a message in the test chat; out marks it as sent by this account.
func (f *deleteInvoker) add(id int, out bool) {
	if f.channel {
		f.msgs[id] = &tg.PeerChannel{ChannelID: testChannelID}
	} else {
		f.msgs[id] = &tg.PeerChat{ChatID: testBasicChatID}
	}
	f.out[id] = out
}

func (f *deleteInvoker) chat() tg.ChatClass {
	if f.channel {
		ch := &tg.Channel{ID: testChannelID, AccessHash: 1, Megagroup: true}
		if f.admin {
			ch.SetAdminRights(tg.ChatAdminRights{DeleteMessages: true})
		}
		return ch
	}
	c := &tg.Chat{ID: testBasicChatID, Title: "Orbit Alerts"}
	if f.admin {
		c.SetAdminRights(tg.ChatAdminRights{DeleteMessages: true})
	}
	return c
}

func (f *deleteInvoker) messages(ids []int) []tg.MessageClass {
	var out []tg.MessageClass
	for _, id := range ids {
		if peer, ok := f.msgs[id]; ok {
			m := &tg.Message{ID: id, PeerID: peer, Message: "alert"}
			m.SetOut(f.out[id]) // GetOut reads the flag bit, not the field
			out = append(out, m)
		} else {
			out = append(out, &tg.MessageEmpty{ID: id})
		}
	}
	return out
}

func (f *deleteInvoker) remove(ids []int) {
	f.deleted = append(f.deleted, slices.Clone(ids))
	if f.skipRevoke {
		return
	}
	for _, id := range ids {
		delete(f.msgs, id)
		delete(f.scheduled, id)
	}
}

func inputIDs(in []tg.InputMessageClass) []int {
	ids := make([]int, len(in))
	for i, m := range in {
		ids[i] = m.(*tg.InputMessageID).ID
	}
	return ids
}

func (f *deleteInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch req := input.(type) {
	case *tg.UsersGetUsersRequest:
		// Not a user: the resolver falls through to the chat probes.
		output.(*tg.UserClassVector).Elems = nil
	case *tg.MessagesGetChatsRequest:
		output.(*tg.MessagesChatsBox).Chats = &tg.MessagesChats{Chats: []tg.ChatClass{f.chat()}}
	case *tg.ChannelsGetChannelsRequest:
		if !f.channel {
			return tgerr.New(400, "CHANNEL_INVALID")
		}
		output.(*tg.MessagesChatsBox).Chats = &tg.MessagesChats{Chats: []tg.ChatClass{f.chat()}}
	case *tg.MessagesGetMessagesRequest:
		if f.rereadErr != nil && len(f.deleted) > 0 {
			return f.rereadErr
		}
		output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{
			Messages: f.messages(inputIDs(req.ID)),
			Chats:    []tg.ChatClass{f.chat()},
		}
	case *tg.ChannelsGetMessagesRequest:
		output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesChannelMessages{
			Messages: f.messages(inputIDs(req.ID)),
			Chats:    []tg.ChatClass{f.chat()},
		}
	case *tg.MessagesDeleteMessagesRequest:
		if !req.Revoke {
			return fmt.Errorf("deleteInvoker: delete without revoke")
		}
		if f.deleteErr != nil {
			return f.deleteErr
		}
		f.remove(req.ID)
		*output.(*tg.MessagesAffectedMessages) = tg.MessagesAffectedMessages{Pts: 1, PtsCount: len(req.ID)}
	case *tg.ChannelsDeleteMessagesRequest:
		if f.deleteErr != nil {
			return f.deleteErr
		}
		f.remove(req.ID)
		*output.(*tg.MessagesAffectedMessages) = tg.MessagesAffectedMessages{Pts: 1, PtsCount: len(req.ID)}
	case *tg.MessagesGetScheduledMessagesRequest:
		if f.rereadErr != nil && len(f.deleted) > 0 {
			return f.rereadErr
		}
		var msgs []tg.MessageClass
		for _, id := range req.ID {
			if f.scheduled[id] {
				m := &tg.Message{ID: id, PeerID: &tg.PeerChat{ChatID: testBasicChatID}}
				m.SetOut(true)
				msgs = append(msgs, m)
			}
		}
		output.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: msgs}
	case *tg.MessagesDeleteScheduledMessagesRequest:
		f.remove(req.ID)
		output.(*tg.UpdatesBox).Updates = &tg.Updates{}
	default:
		return fmt.Errorf("deleteInvoker: unexpected request %T", input)
	}
	return nil
}

func runDelete(t *testing.T, inv *deleteInvoker, ids ...string) (*mcp.CallToolResult, *DeleteMessagesResult) {
	t.Helper()
	h := NewMessageDeleteHandler(tgclient.NewResolver(tg.NewClient(inv)))
	errRes, out, err := h.handle(context.Background(), &mcp.CallToolRequest{}, DeleteMessagesInput{
		ChatID:     testBasicChatID,
		MessageIDs: ids,
		Confirm:    true,
	})
	require.NoError(t, err)
	return errRes, out
}

func statusesOf(out *DeleteMessagesResult) map[string]string {
	m := map[string]string{}
	for _, r := range out.Results {
		m[r.MessageID] = r.Status
	}
	return m
}

func TestDeleteMessagesBasicGroup(t *testing.T) {
	const chatID = testBasicChatID

	t.Run("admin deletes a bot message", func(t *testing.T) {
		inv := newBasicGroupInvoker()
		inv.admin = true
		inv.add(559966, false)
		errRes, out := runDelete(t, inv, "559966")
		require.Nil(t, errRes)
		assert.Equal(t, statusCompleted, out.Status)
		assert.Equal(t, int64(chatID), out.ChatID)
		assert.Equal(t, 1, out.Deleted)
		assert.Equal(t, map[string]string{"559966": statusDeleted}, statusesOf(out))
		assert.NotContains(t, inv.msgs, 559966, "message must be gone after the call")
		assert.Empty(t, out.Hint)
	})

	t.Run("non-admin leaves a bot message untouched", func(t *testing.T) {
		inv := newBasicGroupInvoker()
		inv.add(559966, false)
		errRes, out := runDelete(t, inv, "559966")
		require.Nil(t, errRes)
		assert.Equal(t, map[string]string{"559966": statusForbidden}, statusesOf(out))
		assert.Equal(t, 0, out.Deleted)
		assert.Contains(t, out.Hint, "admin rights")
		assert.Empty(t, inv.deleted, "a message we may not revoke must not be sent (it would vanish only for us)")
	})

	t.Run("own message is deleted without admin rights", func(t *testing.T) {
		inv := newBasicGroupInvoker()
		inv.add(42, true)
		errRes, out := runDelete(t, inv, "42")
		require.Nil(t, errRes)
		assert.Equal(t, map[string]string{"42": statusDeleted}, statusesOf(out))
		assert.NotContains(t, inv.msgs, 42)
	})

	t.Run("already deleted message is not_found and never sent", func(t *testing.T) {
		inv := newBasicGroupInvoker()
		inv.admin = true
		inv.add(42, false)
		_, first := runDelete(t, inv, "42")
		assert.Equal(t, statusDeleted, statusesOf(first)["42"])

		errRes, out := runDelete(t, inv, "42")
		require.Nil(t, errRes)
		assert.Equal(t, map[string]string{"42": statusNotFound}, statusesOf(out))
		assert.Len(t, inv.deleted, 1, "the repeat call must not reach messages.deleteMessages")
	})

	t.Run("message from another chat is not_found and never sent", func(t *testing.T) {
		inv := newBasicGroupInvoker()
		inv.admin = true
		inv.msgs[99] = &tg.PeerUser{UserID: 1}
		errRes, out := runDelete(t, inv, "99")
		require.Nil(t, errRes)
		assert.Equal(t, map[string]string{"99": statusNotFound}, statusesOf(out))
		assert.Empty(t, inv.deleted)
		assert.Contains(t, inv.msgs, 99)
	})

	t.Run("silently skipped revoke is reported forbidden", func(t *testing.T) {
		inv := newBasicGroupInvoker()
		inv.admin = true
		inv.skipRevoke = true
		inv.add(42, false)
		errRes, out := runDelete(t, inv, "42")
		require.Nil(t, errRes)
		assert.Equal(t, map[string]string{"42": statusForbidden}, statusesOf(out))
		assert.Equal(t, 0, out.Deleted)
	})

	t.Run("batch reports each message and dedupes repeats", func(t *testing.T) {
		inv := newBasicGroupInvoker()
		inv.add(1, true)
		inv.add(2, false)
		errRes, out := runDelete(t, inv, "1", "2", "3", "1")
		require.Nil(t, errRes)
		assert.Equal(t, []DeletedMessage{
			{MessageID: "1", Status: statusDeleted},
			{MessageID: "2", Status: statusForbidden},
			{MessageID: "3", Status: statusNotFound},
		}, out.Results)
		assert.Equal(t, 1, out.Deleted)
		assert.Equal(t, [][]int{{1}}, inv.deleted)
	})
}

func TestDeleteMessagesChannelError(t *testing.T) {
	inv := newBasicGroupInvoker()
	inv.channel = true
	inv.admin = true
	inv.deleteErr = tgerr.New(403, "MESSAGE_DELETE_FORBIDDEN")
	inv.add(42, false)
	h := NewMessageDeleteHandler(tgclient.NewResolver(tg.NewClient(inv)))
	_, out, err := h.handle(context.Background(), &mcp.CallToolRequest{}, DeleteMessagesInput{
		ChatID:     testChannelID,
		MessageIDs: []string{"42"},
		Confirm:    true,
	})
	require.Nil(t, out)
	require.ErrorIs(t, err, inv.deleteErr)
	text := failureText("DeleteMessages", err)
	assert.Contains(t, text, "MESSAGE_DELETE_FORBIDDEN")
	assert.Contains(t, text, "admin rights")
}

func TestDeleteMessagesScheduled(t *testing.T) {
	inv := newBasicGroupInvoker()
	inv.scheduled = map[int]bool{7: true}
	errRes, out := runDelete(t, inv, "s:7", "s:8")
	require.Nil(t, errRes)
	assert.Equal(t, map[string]string{"s:7": statusDeleted, "s:8": statusNotFound}, statusesOf(out))
	assert.Equal(t, [][]int{{7}}, inv.deleted)
}

// TestDeleteMessagesUnverified pins that a deletion Telegram accepted but that
// could not be re-read is reported as unverified in a completed result — not
// as a failed deletion, and without sending the deletion again.
func TestDeleteMessagesUnverified(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*deleteInvoker)
		ids   []string
		check string
	}{
		{"regular", func(inv *deleteInvoker) { inv.add(42, true) }, []string{"42", "43"}, "GetMessages which"},
		{"scheduled", func(inv *deleteInvoker) { inv.scheduled = map[int]bool{42: true} }, []string{"s:42", "s:43"}, "include_scheduled=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := newBasicGroupInvoker()
			tc.setup(inv)
			inv.rereadErr = tgerr.New(500, "INTERNAL")
			errRes, out := runDelete(t, inv, tc.ids...)
			require.Nil(t, errRes)
			require.NotNil(t, out)
			assert.Equal(t, statusCompleted, out.Status)
			assert.Equal(t, map[string]string{tc.ids[0]: statusUnverified, tc.ids[1]: statusNotFound}, statusesOf(out))
			assert.Zero(t, out.Deleted, "an unverified deletion is not counted as deleted")
			assert.Contains(t, out.Note, "Telegram accepted the deletion")
			assert.Contains(t, out.Note, "INTERNAL")
			assert.Contains(t, out.Note, tc.check)
			assert.Equal(t, [][]int{{42}}, inv.deleted, "the deletion is sent exactly once")
		})
	}
}

func TestDeleteMessagesConfirmGate(t *testing.T) {
	t.Run("unconfirmed without session never reaches the API", func(t *testing.T) {
		inv := newBasicGroupInvoker()
		inv.add(42, true)
		h := NewMessageDeleteHandler(tgclient.NewResolver(tg.NewClient(inv)))
		errRes, out, err := h.handle(context.Background(), &mcp.CallToolRequest{}, DeleteMessagesInput{
			ChatID:     testBasicChatID,
			MessageIDs: []string{"42"},
		})
		require.NoError(t, err)
		require.Nil(t, out)
		require.NotNil(t, errRes)
		assert.True(t, errRes.IsError)
		assert.Empty(t, inv.deleted)
		assert.Contains(t, inv.msgs, 42)
	})
}
