package resources

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// ChatsHandler handles the telegram://chats resource.
type ChatsHandler struct {
	cache *tgdata.ChatsCache
}

// NewChatsHandler creates a new ChatsHandler that serves the shared chat
// snapshot.
func NewChatsHandler(cache *tgdata.ChatsCache) *ChatsHandler {
	return &ChatsHandler{cache: cache}
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
	// Resource reads carry no progress token, so the load reports none.
	snap, err := h.cache.Load(ctx, nil, false)
	if err != nil {
		return nil, fmt.Errorf("loading chats: %w", err)
	}
	return jsonResource("telegram://chats", tgdata.ChatsList{
		Chats:     snap.Chats,
		Count:     len(snap.Chats),
		Truncated: snap.Truncated,
	})
}
