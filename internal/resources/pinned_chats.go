package resources

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"golang.org/x/sync/singleflight"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// PinnedChatsProvider manages dynamic resources for pinned chats
type PinnedChatsProvider struct {
	client      *tg.Client
	provider    *messages.Provider
	server      *server.MCPServer
	currentURIs []string           // track current pinned resource URIs for cleanup
	sfGroup     singleflight.Group // deduplicates concurrent refresh calls
}

// PinnedChatResource represents a pinned chat resource content
type PinnedChatResource struct {
	Chat     tgdata.ChatInfo    `json:"chat"`
	Messages []messages.Message `json:"messages"`
}

// NewPinnedChatsProvider creates a new PinnedChatsProvider
func NewPinnedChatsProvider(client *tg.Client, provider *messages.Provider, srv *server.MCPServer) *PinnedChatsProvider {
	return &PinnedChatsProvider{
		client:   client,
		provider: provider,
		server:   srv,
	}
}

// RefreshResources updates the list of pinned chat resources.
// Concurrent calls are deduplicated using singleflight.
func (p *PinnedChatsProvider) RefreshResources(ctx context.Context) error {
	_, err, _ := p.sfGroup.Do("refresh", func() (any, error) {
		return nil, p.doRefresh(ctx)
	})
	if err != nil {
		return fmt.Errorf("refreshing pinned resources: %w", err)
	}
	return nil
}

func (p *PinnedChatsProvider) doRefresh(ctx context.Context) error {
	chats, err := tgdata.GetPinnedChats(ctx, p.client)
	if err != nil {
		return fmt.Errorf("getting pinned chats: %w", err)
	}

	// Remove previously added pinned resources
	if len(p.currentURIs) > 0 {
		p.server.DeleteResources(p.currentURIs...)
		p.currentURIs = nil
	}

	var pinnedResources []server.ServerResource
	var newURIs []string

	for _, chat := range chats {
		uri := fmt.Sprintf("telegram://chats/%d", chat.ID)
		chatCopy := chat // capture for closure
		newURIs = append(newURIs, uri)

		pinnedResources = append(pinnedResources, server.ServerResource{
			Resource: mcp.NewResource(
				uri,
				fmt.Sprintf("Messages from %s", chat.Name),
				mcp.WithResourceDescription(fmt.Sprintf("Last 100 messages from chat: %s (%s)", chat.Name, chat.Type)),
				mcp.WithMIMEType("application/json"),
			),
			Handler: func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				return p.handlePinnedChat(ctx, request, chatCopy)
			},
		})
	}

	p.server.AddResources(pinnedResources...)
	p.currentURIs = newURIs
	return nil
}

// handlePinnedChat fetches the last 100 messages for a pinned chat
func (p *PinnedChatsProvider) handlePinnedChat(
	ctx context.Context,
	request mcp.ReadResourceRequest,
	chat tgdata.ChatInfo,
) ([]mcp.ResourceContents, error) {
	opts := messages.FetchOptions{
		Limit: 100,
	}

	lastMessages, err := p.provider.Fetch(ctx, chat.ID, opts)
	if err != nil {
		return nil, fmt.Errorf("fetching messages: %w", err)
	}

	result := PinnedChatResource{
		Chat:     chat,
		Messages: lastMessages.Messages,
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling response: %w", err)
	}

	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      request.Params.URI,
			MIMEType: "application/json",
			Text:     string(data),
		},
	}, nil
}
