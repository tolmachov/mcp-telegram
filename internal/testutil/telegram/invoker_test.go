package telegram

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTypedInvoker(t *testing.T) {
	invoker := New(Typed(func(_ context.Context, request *tg.MessagesReadHistoryRequest, output *tg.MessagesAffectedMessages) error {
		assert.Equal(t, 42, request.MaxID)
		output.Pts = 7
		return nil
	}))
	client := tg.NewClient(invoker)
	result, err := client.MessagesReadHistory(t.Context(), &tg.MessagesReadHistoryRequest{MaxID: 42})
	require.NoError(t, err)
	assert.Equal(t, 7, result.Pts)
	assert.Len(t, invoker.RequestTypes(), 1)
	assert.Zero(t, invoker.Remaining())
}
