package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// parseChatIDFromURI extracts the chat ID from a telegram://chat/{chat_id} URI.
func parseChatIDFromURI(uri string) (int64, error) {
	uri = strings.TrimPrefix(uri, "telegram://chat/")
	if idx := strings.Index(uri, "/"); idx != -1 {
		uri = uri[:idx]
	}
	if idx := strings.Index(uri, "?"); idx != -1 {
		uri = uri[:idx]
	}
	chatID, err := strconv.ParseInt(uri, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing chat_id: %w", err)
	}
	return chatID, nil
}

// ChatInfoHandler handles the telegram://chat/{chat_id} resource template
type ChatInfoHandler struct {
	client *tg.Client
}

// NewChatInfoHandler creates a new ChatInfoHandler
func NewChatInfoHandler(client *tg.Client) *ChatInfoHandler {
	return &ChatInfoHandler{client: client}
}

// Template returns the MCP resource template definition
func (h *ChatInfoHandler) Template() mcp.ResourceTemplate {
	return mcp.NewResourceTemplate(
		"telegram://chat/{chat_id}",
		"Chat Info",
		mcp.WithTemplateDescription("Information about a specific chat, group, or channel"),
		mcp.WithTemplateMIMEType("application/json"),
	)
}

// Handle processes the telegram://chat/{chat_id} resource request
func (h *ChatInfoHandler) Handle(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	chatID, err := parseChatIDFromURI(request.Params.URI)
	if err != nil {
		return nil, fmt.Errorf("parsing URI: %w", err)
	}

	info, err := tgdata.GetChatInfo(ctx, h.client, chatID)
	if err != nil {
		return nil, err
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling chat info: %w", err)
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      request.Params.URI,
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}
