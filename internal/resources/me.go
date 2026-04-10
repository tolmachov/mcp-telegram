package resources

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// MeHandler handles the telegram://me resource.
type MeHandler struct {
	client *tg.Client
}

// NewMeHandler creates a new MeHandler.
func NewMeHandler(client *tg.Client) *MeHandler {
	return &MeHandler{client: client}
}

// Register adds the resource to the MCP server.
func (h *MeHandler) Register(s *mcp.Server) {
	s.AddResource(&mcp.Resource{
		URI:         "telegram://me",
		Name:        "Current User",
		Description: "Information about the currently authenticated Telegram user",
		MIMEType:    "application/json",
	}, h.handle)
}

func (h *MeHandler) handle(ctx context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	info, err := tgdata.GetCurrentUser(ctx, h.client)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling user info: %w", err)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      "telegram://me",
			MIMEType: "application/json",
			Text:     string(data),
		}},
	}, nil
}
