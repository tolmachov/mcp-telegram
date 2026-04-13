package resources

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// ChatsHandler handles the telegram://chats resource.
type ChatsHandler struct {
	client *tg.Client
}

// NewChatsHandler creates a new ChatsHandler.
func NewChatsHandler(client *tg.Client) *ChatsHandler {
	return &ChatsHandler{client: client}
}

// Register adds the resource to the MCP server.
func (h *ChatsHandler) Register(s *mcp.Server) {
	s.AddResource(&mcp.Resource{
		URI:         "telegram://chats",
		Name:        "Chats List",
		Description: "List of all chats, groups, and channels",
		MIMEType:    "application/json",
	}, h.handle)
}

func (h *ChatsHandler) handle(ctx context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	// Resource handlers don't get a progress token in the same way tools do —
	// reads are typically fast and clients don't expect progress on them.
	// If the progress channel is needed in the future, capture req.Session
	// and call NotifyProgress with a server-generated token.
	onProgress := func(_ int, _ string) {}

	result, err := tgdata.GetChats(ctx, h.client, onProgress)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling chats: %w", err)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      "telegram://chats",
			MIMEType: "application/json",
			Text:     string(data),
		}},
	}, nil
}
