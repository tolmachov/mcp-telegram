package prompts

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func promptRequest(args map[string]string) *mcp.GetPromptRequest {
	return &mcp.GetPromptRequest{Params: &mcp.GetPromptParams{Arguments: args}}
}

func promptText(t *testing.T, result *mcp.GetPromptResult) string {
	t.Helper()
	require.Len(t, result.Messages, 1)
	content, ok := result.Messages[0].Content.(*mcp.TextContent)
	require.True(t, ok)
	return content.Text
}

func TestRegisterAndPromptRendering(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	Register(server)

	digest, err := handleDailyDigest(context.Background(), promptRequest(nil))
	require.NoError(t, err)
	assert.Contains(t, promptText(t, digest), "last day")
	digest, err = handleDailyDigest(context.Background(), promptRequest(map[string]string{"period": "month"}))
	require.NoError(t, err)
	assert.Contains(t, promptText(t, digest), "last month")

	_, err = handleChatCatchup(context.Background(), promptRequest(nil))
	assert.ErrorContains(t, err, "chat")
	catchup, err := handleChatCatchup(context.Background(), promptRequest(map[string]string{"chat": "Orbit"}))
	require.NoError(t, err)
	assert.Contains(t, promptText(t, catchup), "last week")
	catchup, err = handleChatCatchup(context.Background(), promptRequest(map[string]string{"chat": "@orbit", "period": "day"}))
	require.NoError(t, err)
	assert.Contains(t, promptText(t, catchup), "@orbit")

	for name, args := range map[string]map[string]string{
		"missing chat":  {"query": "deadline", "reply": "yes"},
		"missing query": {"chat": "Orbit", "reply": "yes"},
		"missing reply": {"chat": "Orbit", "query": "deadline"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := handleFindAndReply(context.Background(), promptRequest(args))
			assert.Error(t, err)
		})
	}
	result, err := handleFindAndReply(context.Background(), promptRequest(map[string]string{
		"chat": "Orbit", "query": "deadline", "reply": "Friday works",
	}))
	require.NoError(t, err)
	text := promptText(t, result)
	assert.Contains(t, text, "deadline")
	assert.Contains(t, text, "Friday works")
}
