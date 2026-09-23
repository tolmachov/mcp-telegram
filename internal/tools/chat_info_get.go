package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// ChatInfoGetHandler handles the GetChatInfo tool.
type ChatInfoGetHandler struct {
	peers *tgclient.Resolver
}

// NewChatInfoGetHandler creates a new ChatInfoGetHandler.
func NewChatInfoGetHandler(peers *tgclient.Resolver) *ChatInfoGetHandler {
	return &ChatInfoGetHandler{peers: peers}
}

// GetChatInfoInput is the input for the GetChatInfo tool.
type GetChatInfoInput struct {
	ChatID int64 `json:"chat_id" jsonschema:"The chat ID to get information about"`
}

// Register adds the tool to the MCP server.
func (h *ChatInfoGetHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name:        "GetChatInfo",
		Description: "Get detailed information about a specific chat, group, or channel including member count, description, and settings. Requires a chat ID — use SearchChats to find one.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *ChatInfoGetHandler) handle(ctx context.Context, _ *mcp.CallToolRequest, in GetChatInfoInput) (*mcp.CallToolResult, *tgdata.ChatFullInfo, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}

	info, err := tgdata.GetChatInfo(ctx, h.peers, in.ChatID)
	if err != nil {
		return nil, nil, failed("get chat info", err)
	}
	return nil, info, nil
}
