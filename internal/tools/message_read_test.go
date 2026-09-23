package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	telegramfake "github.com/tolmachov/mcp-telegram/internal/testutil/telegram"
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
	h := NewMessageReadHandler(tg.NewClient(inv))

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
	h := NewMessageReadHandler(tg.NewClient(inv))

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
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetHistoryRequest, out *tg.MessagesMessagesBox) error {
			assert.Equal(t, 1, req.Limit)
			out.Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{&tg.MessageService{ID: 77}}}
			return nil
		}),
		telegramfake.Typed(func(_ context.Context, req *tg.ChannelsReadHistoryRequest, out *tg.BoolBox) error {
			assert.Equal(t, 77, req.MaxID)
			out.Bool = &tg.BoolTrue{}
			return nil
		}),
	)
	h := NewMessageReadHandler(tg.NewClient(inv))

	errRes, out, err := h.handle(t.Context(), &mcp.CallToolRequest{}, MarkAsReadInput{
		ChatIDs: []int64{channelID},
	})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.NotNil(t, out)
	assert.Equal(t, 1, out.Successful)
	assert.Zero(t, inv.Remaining())
}
