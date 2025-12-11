package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// ChatInfoGetHandler handles the GetChatInfo tool
type ChatInfoGetHandler struct {
	client *tg.Client
}

// NewChatInfoGetHandler creates a new ChatInfoGetHandler
func NewChatInfoGetHandler(client *tg.Client) *ChatInfoGetHandler {
	return &ChatInfoGetHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *ChatInfoGetHandler) Tool() mcp.Tool {
	return mcp.NewTool("GetChatInfo",
		mcp.WithDescription("Get detailed information about a specific chat, group, or channel."),
		mcp.WithNumber("chat_id",
			mcp.Description("The chat ID to get information about"),
			mcp.Required(),
		),
	)
}

// Handle processes the GetChatInfo tool request
func (h *ChatInfoGetHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatID := mcp.ParseInt64(request, "chat_id", 0)
	if chatID == 0 {
		return mcp.NewToolResultError("chat_id is required"), nil
	}

	info, err := tgdata.GetChatInfo(ctx, h.client, chatID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get chat info: %v", err)), nil
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to marshal chat info: %v", err)), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}
