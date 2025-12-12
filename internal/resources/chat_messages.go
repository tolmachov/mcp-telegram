package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
)

// ChatMessagesHandler handles the telegram://chat/{chat_id}/messages resource template
type ChatMessagesHandler struct {
	provider *messages.Provider
}

// NewChatMessagesHandler creates a new ChatMessagesHandler
func NewChatMessagesHandler(provider *messages.Provider) *ChatMessagesHandler {
	return &ChatMessagesHandler{
		provider: provider,
	}
}

// Template returns the MCP resource template definition
func (h *ChatMessagesHandler) Template() mcp.ResourceTemplate {
	return mcp.NewResourceTemplate(
		"telegram://chat/{chat_id}/messages?limit={limit}&offset_id={offset_id}",
		"Chat Messages",
		mcp.WithTemplateDescription("Messages from a specific chat. Parameters: limit (default 50, max 100), offset_id (message ID to start from for pagination)."),
		mcp.WithTemplateMIMEType("application/json"),
	)
}

// Handle processes the telegram://chat/{chat_id}/messages resource request
func (h *ChatMessagesHandler) Handle(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	chatID, opts, err := parseMessagesURI(request.Params.URI)
	if err != nil {
		return nil, fmt.Errorf("parsing URI: %w", err)
	}

	result, err := h.provider.Fetch(ctx, chatID, opts)
	if err != nil {
		return nil, err
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling messages: %w", err)
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      request.Params.URI,
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}

func parseMessagesURI(uri string) (chatID int64, opts messages.FetchOptions, err error) {
	opts = messages.DefaultFetchOptions()

	parsed, err := url.Parse(uri)
	if err != nil {
		return 0, opts, fmt.Errorf("parsing URI: %w", err)
	}

	path := strings.TrimPrefix(parsed.Host+parsed.Path, "chat/")
	path = strings.TrimSuffix(path, "/messages")
	chatID, err = strconv.ParseInt(path, 10, 64)
	if err != nil {
		return 0, opts, fmt.Errorf("parsing chat_id: %w", err)
	}

	query := parsed.Query()

	if limitStr := query.Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			opts.Limit = l
			if opts.Limit > 100 {
				opts.Limit = 100
			}
		}
	}

	if offsetStr := query.Get("offset_id"); offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil {
			opts.OffsetID = o
		}
	}

	if unreadStr := query.Get("unread_only"); unreadStr != "" {
		opts.UnreadOnly = unreadStr == "true" || unreadStr == "1"
	}

	return chatID, opts, nil
}
