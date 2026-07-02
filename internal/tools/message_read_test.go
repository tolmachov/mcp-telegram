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
	require.NoError(t, err)
	require.Nil(t, out, "all chats failed with a non-flood error, so the result collapses to an error")
	require.NotNil(t, errRes)
	assert.True(t, errRes.IsError)
	// Every chat was attempted (no early stop), so the last one appears too.
	assert.Contains(t, toolResultText(errRes), "chat_id=300")
}
