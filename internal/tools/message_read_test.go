package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	telegramfake "github.com/tolmachov/mcp-telegram/internal/testutil/telegram"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// staticErrInvoker fails every MTProto call with the same error, so MarkAsRead's
// first per-chat resolve already returns it.
type staticErrInvoker struct {
	err   error
	calls int
}

func (f *staticErrInvoker) Invoke(_ context.Context, _ bin.Encoder, _ bin.Decoder) error {
	f.calls++
	return f.err
}

func TestMarkAsReadFloodWaitStopsBatch(t *testing.T) {
	flood := &tgerr.Error{Code: 420, Message: "FLOOD_WAIT_30", Type: "FLOOD_WAIT", Argument: 30}
	inv := &staticErrInvoker{err: flood}
	h := NewMessageReadHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv)))

	errRes, out, err := h.handle(context.Background(), &mcp.CallToolRequest{}, MarkAsReadInput{
		ChatIDs: []int64{100, 200, 300},
	})
	require.NoError(t, err)
	require.Nil(t, errRes, "a flood wait yields a structured result, not a bare error")
	require.NotNil(t, out)

	// The batch stopped after the first chat flooded: the rest are skipped, not
	// hammered with more requests.
	assert.Equal(t, 1, out.Failed)
	assert.Equal(t, []int64{200, 300}, out.SkippedIDs)
	assert.Contains(t, out.Warning, "30 seconds")
	assert.Len(t, out.Failures, 1)
	assert.Equal(t, int64(100), out.Failures[0].ChatID)
}

func TestMarkAsReadNonFloodErrorContinuesBatch(t *testing.T) {
	inv := &staticErrInvoker{err: errors.New("boom")}
	h := NewMessageReadHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv)))

	errRes, out, err := h.handle(context.Background(), &mcp.CallToolRequest{}, MarkAsReadInput{
		ChatIDs: []int64{100, 200, 300},
	})
	require.Nil(t, errRes)
	require.Nil(t, out, "all chats failed with a non-flood error, so the result collapses to an error")
	require.Error(t, err)
	// Every chat was attempted (no early stop), so the last one appears too.
	assert.Contains(t, failureText("MarkAsRead", err), "chat_id=300")
}

func TestMarkAsReadChannelUsesCurrentTopMessage(t *testing.T) {
	const channelID = int64(99)
	inv := telegramfake.New(
		notUserStep(t, channelID),
		telegramfake.Typed(func(_ context.Context, req *tg.ChannelsGetChannelsRequest, out *tg.MessagesChatsBox) error {
			assert.Equal(t, channelID, req.ID[0].(*tg.InputChannel).ChannelID)
			out.Chats = &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Channel{ID: channelID, AccessHash: 123}}}
			return nil
		}),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetPeerDialogsRequest, out *tg.MessagesPeerDialogs) error {
			require.Len(t, req.Peers, 1)
			out.Dialogs = []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: channelID}, TopMessage: 77}}
			return nil
		}),
		telegramfake.Typed(func(_ context.Context, req *tg.ChannelsReadHistoryRequest, out *tg.BoolBox) error {
			assert.Equal(t, 77, req.MaxID)
			out.Bool = &tg.BoolTrue{}
			return nil
		}),
	)
	h := NewMessageReadHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv)))

	errRes, out, err := h.handle(t.Context(), &mcp.CallToolRequest{}, MarkAsReadInput{
		ChatIDs: []int64{channelID},
	})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.NotNil(t, out)
	assert.Equal(t, 1, out.Successful)
	assert.Zero(t, inv.Remaining())
}

// TestMarkAsReadLooksUpChannelTopsInOneCall verifies every channel's top
// message comes from a single messages.getPeerDialogs call, a channel without
// a dialog is left alone, and a basic group is read without one.
func TestMarkAsReadLooksUpChannelTopsInOneCall(t *testing.T) {
	const firstID, secondID, groupID = int64(91), int64(92), int64(93)
	inv := telegramfake.New(
		notUserStep(t, firstID),
		resolveChannelStep(t, firstID, 1),
		notUserStep(t, secondID),
		resolveChannelStep(t, secondID, 2),
		notUserStep(t, groupID),
		telegramfake.Typed(func(_ context.Context, _ *tg.ChannelsGetChannelsRequest, _ *tg.MessagesChatsBox) error {
			return tgerr.New(400, "CHANNEL_INVALID")
		}),
		telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetChatsRequest, out *tg.MessagesChatsBox) error {
			out.Chats = &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Chat{ID: groupID}}}
			return nil
		}),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetPeerDialogsRequest, out *tg.MessagesPeerDialogs) error {
			require.Len(t, req.Peers, 2)
			// Only the first channel has a dialog.
			out.Dialogs = []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: firstID}, TopMessage: 5}}
			return nil
		}),
		telegramfake.Typed(func(_ context.Context, req *tg.ChannelsReadHistoryRequest, out *tg.BoolBox) error {
			assert.Equal(t, firstID, req.Channel.(*tg.InputChannel).ChannelID)
			assert.Equal(t, 5, req.MaxID)
			out.Bool = &tg.BoolTrue{}
			return nil
		}),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesReadHistoryRequest, out *tg.MessagesAffectedMessages) error {
			assert.Equal(t, &tg.InputPeerChat{ChatID: groupID}, req.Peer)
			return nil
		}),
	)
	h := NewMessageReadHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv)))

	errRes, out, err := h.handle(t.Context(), &mcp.CallToolRequest{}, MarkAsReadInput{
		ChatIDs: []int64{firstID, secondID, groupID},
	})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.NotNil(t, out)
	assert.Equal(t, 3, out.Successful)
	assert.Equal(t, []int64{secondID}, out.NothingToReadIDs, "the result says which channel had nothing to mark")
	assert.Zero(t, inv.Remaining())
}

// TestMarkAsReadReportsAnUnacknowledgedRead pins that a channel read Telegram
// answers with false fails that chat instead of counting as read.
func TestMarkAsReadReportsAnUnacknowledgedRead(t *testing.T) {
	const channelID, groupID = int64(91), int64(93)
	script := []telegramfake.InvokeFunc{notUserStep(t, channelID), resolveChannelStep(t, channelID, 1)}
	script = append(script, basicGroupSteps(t, groupID)...)
	script = append(script,
		telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetPeerDialogsRequest, out *tg.MessagesPeerDialogs) error {
			out.Dialogs = []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: channelID}, TopMessage: 5}}
			return nil
		}),
		telegramfake.Typed(func(_ context.Context, _ *tg.ChannelsReadHistoryRequest, out *tg.BoolBox) error {
			out.Bool = &tg.BoolFalse{}
			return nil
		}),
		telegramfake.Typed(func(_ context.Context, _ *tg.MessagesReadHistoryRequest, _ *tg.MessagesAffectedMessages) error {
			return nil
		}),
	)
	inv := telegramfake.New(script...)
	h := NewMessageReadHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv)))

	_, out, err := h.handle(t.Context(), &mcp.CallToolRequest{}, MarkAsReadInput{ChatIDs: []int64{channelID, groupID}})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, []int64{groupID}, out.SuccessIDs)
	require.Len(t, out.Failures, 1)
	assert.Equal(t, channelID, out.Failures[0].ChatID)
	assert.Contains(t, out.Failures[0].Error, "did not acknowledge the read")
	assert.Zero(t, inv.Remaining())
}

// TestMarkAsReadCollapsesIdenticalFailures pins that chats failing with the
// same error share one line of the failure instead of repeating it per chat.
func TestMarkAsReadCollapsesIdenticalFailures(t *testing.T) {
	const firstID, secondID = int64(91), int64(92)
	inv := telegramfake.New(
		notUserStep(t, firstID),
		resolveChannelStep(t, firstID, 1),
		notUserStep(t, secondID),
		resolveChannelStep(t, secondID, 2),
		telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetPeerDialogsRequest, _ *tg.MessagesPeerDialogs) error {
			return tgerr.New(500, "INTERNAL_SERVER_ERROR")
		}),
	)
	h := NewMessageReadHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv)))

	_, out, err := h.handle(t.Context(), &mcp.CallToolRequest{}, MarkAsReadInput{ChatIDs: []int64{firstID, secondID}})
	require.Nil(t, out)
	require.Error(t, err)
	txt := failureText("MarkAsRead", err)
	assert.Contains(t, txt, "chat_id=91, 92: getting channel top messages: rpc error code 500: INTERNAL_SERVER_ERROR")
	assert.Equal(t, 1, strings.Count(txt, "INTERNAL_SERVER_ERROR"), txt)
}

// basicGroupSteps answers the resolver's probes for id as a basic group.
func basicGroupSteps(t *testing.T, id int64) []telegramfake.InvokeFunc {
	t.Helper()
	return []telegramfake.InvokeFunc{
		notUserStep(t, id),
		telegramfake.Typed(func(_ context.Context, _ *tg.ChannelsGetChannelsRequest, _ *tg.MessagesChatsBox) error {
			return tgerr.New(400, "CHANNEL_INVALID")
		}),
		telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetChatsRequest, out *tg.MessagesChatsBox) error {
			out.Chats = &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Chat{ID: id}}}
			return nil
		}),
	}
}

// TestMarkAsReadIsolatesBadChannel verifies one channel failing the shared
// top-message lookup fails only itself: each channel then looks up its own,
// and the result reports the mix.
func TestMarkAsReadIsolatesBadChannel(t *testing.T) {
	const goodID, badID = int64(91), int64(92)
	inv := telegramfake.New(
		notUserStep(t, goodID),
		resolveChannelStep(t, goodID, 1),
		notUserStep(t, badID),
		resolveChannelStep(t, badID, 2),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetPeerDialogsRequest, _ *tg.MessagesPeerDialogs) error {
			require.Len(t, req.Peers, 2)
			return tgerr.New(400, "CHANNEL_PRIVATE")
		}),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetPeerDialogsRequest, out *tg.MessagesPeerDialogs) error {
			require.Len(t, req.Peers, 1)
			out.Dialogs = []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: goodID}, TopMessage: 5}}
			return nil
		}),
		telegramfake.Typed(func(_ context.Context, req *tg.ChannelsReadHistoryRequest, out *tg.BoolBox) error {
			assert.Equal(t, goodID, req.Channel.(*tg.InputChannel).ChannelID)
			assert.Equal(t, 5, req.MaxID)
			out.Bool = &tg.BoolTrue{}
			return nil
		}),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetPeerDialogsRequest, _ *tg.MessagesPeerDialogs) error {
			require.Len(t, req.Peers, 1)
			return tgerr.New(400, "CHANNEL_PRIVATE")
		}),
	)
	h := NewMessageReadHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv)))

	errRes, out, err := h.handle(t.Context(), &mcp.CallToolRequest{}, MarkAsReadInput{ChatIDs: []int64{goodID, badID}})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.NotNil(t, out)
	assert.Equal(t, []int64{goodID}, out.SuccessIDs)
	require.Len(t, out.Failures, 1)
	assert.Equal(t, badID, out.Failures[0].ChatID)
	assert.Contains(t, out.Failures[0].Error, "CHANNEL_PRIVATE")
	assert.Equal(t, 2, out.TotalChats)
	assert.Empty(t, out.SkippedIDs)
	assert.Equal(t, "1 of the chats could not be marked as read; failures says why.", out.Warning)
	assert.Zero(t, inv.Remaining())
}

// TestMarkAsReadSharedLookupFailureFailsEveryChannel verifies a shared
// top-message lookup failing for a reason that is neither systemic nor about
// one channel fails every channel with it — without repeating the lookup per
// channel — while the batch's other chats are still read.
func TestMarkAsReadSharedLookupFailureFailsEveryChannel(t *testing.T) {
	const firstID, secondID, groupID = int64(91), int64(92), int64(93)
	script := []telegramfake.InvokeFunc{
		notUserStep(t, firstID),
		resolveChannelStep(t, firstID, 1),
		notUserStep(t, secondID),
		resolveChannelStep(t, secondID, 2),
	}
	script = append(script, basicGroupSteps(t, groupID)...)
	script = append(script,
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetPeerDialogsRequest, _ *tg.MessagesPeerDialogs) error {
			require.Len(t, req.Peers, 2)
			return tgerr.New(500, "INTERNAL_SERVER_ERROR")
		}),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesReadHistoryRequest, _ *tg.MessagesAffectedMessages) error {
			assert.Equal(t, &tg.InputPeerChat{ChatID: groupID}, req.Peer)
			return nil
		}),
	)
	inv := telegramfake.New(script...)
	h := NewMessageReadHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv)))

	errRes, out, err := h.handle(t.Context(), &mcp.CallToolRequest{}, MarkAsReadInput{ChatIDs: []int64{firstID, secondID, groupID}})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.NotNil(t, out)
	assert.Equal(t, []int64{groupID}, out.SuccessIDs)
	require.Len(t, out.Failures, 2)
	for i, id := range []int64{firstID, secondID} {
		assert.Equal(t, id, out.Failures[i].ChatID)
		assert.Contains(t, out.Failures[i].Error, "INTERNAL_SERVER_ERROR")
	}
	assert.Equal(t, 3, out.TotalChats)
	assert.Empty(t, out.SkippedIDs)
	assert.Zero(t, inv.Remaining(), "no per-channel lookup follows")
}

// TestMarkAsReadFloodWaitMidReadSkipsRest verifies a flood wait while reading
// one chat stops the batch there, keeping the chats already read.
func TestMarkAsReadFloodWaitMidReadSkipsRest(t *testing.T) {
	flood := &tgerr.Error{Code: 420, Message: "FLOOD_WAIT_30", Type: "FLOOD_WAIT", Argument: 30}
	var script []telegramfake.InvokeFunc
	for _, id := range []int64{1, 2, 3} {
		script = append(script, basicGroupSteps(t, id)...)
	}
	script = append(script,
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesReadHistoryRequest, _ *tg.MessagesAffectedMessages) error {
			assert.Equal(t, &tg.InputPeerChat{ChatID: 1}, req.Peer)
			return nil
		}),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesReadHistoryRequest, _ *tg.MessagesAffectedMessages) error {
			assert.Equal(t, &tg.InputPeerChat{ChatID: 2}, req.Peer)
			return flood
		}),
	)
	inv := telegramfake.New(script...)
	h := NewMessageReadHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv)))

	errRes, out, err := h.handle(t.Context(), &mcp.CallToolRequest{}, MarkAsReadInput{ChatIDs: []int64{1, 2, 3}})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.NotNil(t, out)
	assert.Equal(t, []int64{1}, out.SuccessIDs)
	require.Len(t, out.Failures, 1)
	assert.Equal(t, int64(2), out.Failures[0].ChatID)
	assert.Equal(t, []int64{3}, out.SkippedIDs)
	assert.Contains(t, out.Warning, "30 seconds")
	assert.Zero(t, inv.Remaining())
}

// TestMarkAsReadDeadSessionStopsBatch verifies a systemic failure of the
// shared top-message lookup skips every remaining chat without calling
// Telegram again.
func TestMarkAsReadDeadSessionStopsBatch(t *testing.T) {
	const channelID, groupID = int64(91), int64(93)
	script := []telegramfake.InvokeFunc{notUserStep(t, channelID), resolveChannelStep(t, channelID, 1)}
	script = append(script, basicGroupSteps(t, groupID)...)
	script = append(script, telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetPeerDialogsRequest, _ *tg.MessagesPeerDialogs) error {
		// The verdict a refusal by the home DC reaches the call as.
		return fmt.Errorf("%w: %w", tgclient.ErrSessionUnauthorized, tgerr.New(401, "AUTH_KEY_UNREGISTERED"))
	}))
	inv := telegramfake.New(script...)
	h := NewMessageReadHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv)))

	errRes, out, err := h.handle(t.Context(), &mcp.CallToolRequest{}, MarkAsReadInput{ChatIDs: []int64{channelID, groupID}})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.NotNil(t, out)
	assert.Zero(t, out.TotalChats)
	assert.Equal(t, []int64{channelID, groupID}, out.SkippedIDs)
	assert.Equal(t, "The batch stopped early, leaving the chats in skipped_ids untouched: getting channel top messages: telegram session is not authorized: rpc error code 401: AUTH_KEY_UNREGISTERED.", out.Warning,
		"the warning renders the cause as a tool failure does; the server explains the dead session")
	assert.Len(t, inv.RequestTypes(), len(script), "no call follows the systemic failure")
}
