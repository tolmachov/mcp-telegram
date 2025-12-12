package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
)

// MessagesGetHandler handles the GetMessages tool
type MessagesGetHandler struct {
	provider *messages.Provider
}

// NewMessagesGetHandler creates a new MessagesGetHandler
func NewMessagesGetHandler(provider *messages.Provider) *MessagesGetHandler {
	return &MessagesGetHandler{
		provider: provider,
	}
}

// Tool returns the MCP tool definition
func (h *MessagesGetHandler) Tool() mcp.Tool {
	return mcp.NewTool("GetMessages",
		mcp.WithDescription("Get messages from a specific chat."),
		mcp.WithNumber("chat_id",
			mcp.Description("The chat ID to get messages from"),
			mcp.Required(),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of messages to return (default 50, max 100)"),
		),
		mcp.WithNumber("offset_id",
			mcp.Description("Message ID to start from for pagination"),
		),
		mcp.WithBoolean("unread_only",
			mcp.Description("Only return unread messages"),
		),
	)
}

// Handle processes the GetMessages tool request
func (h *MessagesGetHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatID := mcp.ParseInt64(request, "chat_id", 0)
	if chatID == 0 {
		return mcp.NewToolResultError("chat_id is required"), nil
	}

	opts := messages.DefaultFetchOptions()

	if limit := int(mcp.ParseInt64(request, "limit", 0)); limit > 0 {
		opts.Limit = limit
		if opts.Limit > 100 {
			opts.Limit = 100
		}
	}

	if offsetID := int(mcp.ParseInt64(request, "offset_id", 0)); offsetID > 0 {
		opts.OffsetID = offsetID
	}

	opts.UnreadOnly = mcp.ParseBoolean(request, "unread_only", false)

	result, err := h.provider.Fetch(ctx, chatID, opts)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get messages: %v", err)), nil
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to marshal messages: %v", err)), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}
