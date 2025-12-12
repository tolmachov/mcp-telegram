package resources

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// ChatsHandler handles the telegram://chats resource
type ChatsHandler struct {
	client *tg.Client
}

// NewChatsHandler creates a new ChatsHandler
func NewChatsHandler(client *tg.Client) *ChatsHandler {
	return &ChatsHandler{client: client}
}

// Resource returns the MCP resource definition
func (h *ChatsHandler) Resource() mcp.Resource {
	return mcp.NewResource(
		"telegram://chats",
		"Chats List",
		mcp.WithResourceDescription("List of all chats, groups, and channels"),
		mcp.WithMIMEType("application/json"),
	)
}

// Handle processes the telegram://chats resource request
func (h *ChatsHandler) Handle(ctx context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
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
		return nil, err
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling chats: %w", err)
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      "telegram://chats",
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}
