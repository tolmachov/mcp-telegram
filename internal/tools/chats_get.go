package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// ChatsGetHandler handles the GetChats tool
type ChatsGetHandler struct {
	client *tg.Client
}

// NewChatsGetHandler creates a new ChatsGetHandler
func NewChatsGetHandler(client *tg.Client) *ChatsGetHandler {
	return &ChatsGetHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *ChatsGetHandler) Tool() mcp.Tool {
	return mcp.NewTool("GetChats",
		mcp.WithDescription("Get a list of all chats, groups, and channels."),
	)
}

// Handle processes the GetChats tool request
func (h *ChatsGetHandler) Handle(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	onProgress := func(current int, message string) {
		if srv := server.ServerFromContext(ctx); srv != nil {
			_ = srv.SendNotificationToClient(ctx, "notifications/progress", map[string]any{
				"progress": current,
				"message":  message,
			})
		}
	}

	result, err := tgdata.GetChats(ctx, h.client, onProgress)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get chats: %v", err)), nil
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to marshal chats: %v", err)), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}
