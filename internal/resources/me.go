package resources

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// MeHandler handles the telegram://me resource
type MeHandler struct {
	client *tg.Client
}

// NewMeHandler creates a new MeHandler
func NewMeHandler(client *tg.Client) *MeHandler {
	return &MeHandler{client: client}
}

// Resource returns the MCP resource definition
func (h *MeHandler) Resource() mcp.Resource {
	return mcp.NewResource(
		"telegram://me",
		"Current User",
		mcp.WithResourceDescription("Information about the currently authenticated Telegram user"),
		mcp.WithMIMEType("application/json"),
	)
}

// Handle processes the telegram://me resource request
func (h *MeHandler) Handle(ctx context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	info, err := tgdata.GetCurrentUser(ctx, h.client)
	if err != nil {
		return nil, err
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling user info: %w", err)
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      "telegram://me",
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}
