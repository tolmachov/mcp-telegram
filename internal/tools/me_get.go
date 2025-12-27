package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// MeGetHandler handles the GetMe tool
type MeGetHandler struct {
	client *tg.Client
}

// NewMeGetHandler creates a new MeGetHandler
func NewMeGetHandler(client *tg.Client) *MeGetHandler {
	return &MeGetHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *MeGetHandler) Tool() mcp.Tool {
	return mcp.NewTool("GetMe",
		mcp.WithDescription("Get information about the currently authenticated Telegram user."),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

// Handle processes the GetMe tool request
func (h *MeGetHandler) Handle(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	info, err := tgdata.GetCurrentUser(ctx, h.client)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get current user: %v", err)), nil
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to marshal user info: %v", err)), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}
