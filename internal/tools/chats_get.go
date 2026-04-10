package tools

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// ChatsGetHandler handles the GetChats tool.
type ChatsGetHandler struct {
	client *tg.Client
}

// NewChatsGetHandler creates a new ChatsGetHandler.
func NewChatsGetHandler(client *tg.Client) *ChatsGetHandler {
	return &ChatsGetHandler{client: client}
}

// GetChatsInput is the (empty) input for the GetChats tool.
type GetChatsInput struct{}

// Register adds the tool to the MCP server.
func (h *ChatsGetHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "GetChats",
		Description: "Get a list of ALL chats, groups, and channels. May be slow for accounts with many chats. To find a specific chat by name, use SearchChats instead.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *ChatsGetHandler) handle(ctx context.Context, req *mcp.CallToolRequest, _ GetChatsInput) (*mcp.CallToolResult, *tgdata.ChatsList, error) {
	onProgress := func(current int, message string) {
		sendProgress(ctx, req, float64(current), 0, message)
	}

	result, err := tgdata.GetChats(ctx, h.client, onProgress)
	if err != nil {
		return errResult(fmt.Sprintf("Failed to get chats: %v", err)), nil, nil
	}
	return nil, result, nil
}
