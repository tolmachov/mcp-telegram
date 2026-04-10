package tools

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// ChatInfoGetHandler handles the GetChatInfo tool.
type ChatInfoGetHandler struct {
	client *tg.Client
}

// NewChatInfoGetHandler creates a new ChatInfoGetHandler.
func NewChatInfoGetHandler(client *tg.Client) *ChatInfoGetHandler {
	return &ChatInfoGetHandler{client: client}
}

// GetChatInfoInput is the input for the GetChatInfo tool.
type GetChatInfoInput struct {
	ChatID int64 `json:"chat_id" jsonschema:"The chat ID to get information about"`
}

// Register adds the tool to the MCP server.
func (h *ChatInfoGetHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "GetChatInfo",
		Description: "Get detailed information about a specific chat, group, or channel including member count, description, and settings. Requires a chat ID — use SearchChats to find one.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *ChatInfoGetHandler) handle(ctx context.Context, _ *mcp.CallToolRequest, in GetChatInfoInput) (*mcp.CallToolResult, *tgdata.ChatFullInfo, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}

	info, err := tgdata.GetChatInfo(ctx, h.client, in.ChatID)
	if err != nil {
		return errResult(fmt.Sprintf("Failed to get chat info: %v", err)), nil, nil
	}
	return nil, info, nil
}
