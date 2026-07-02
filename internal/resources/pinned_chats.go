package resources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/singleflight"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// PinnedChatsProvider manages dynamic resources for pinned chats.
//
// The MCP spec encourages servers to notify clients when resources change.
// modelcontextprotocol/go-sdk auto-emits notifications/resources/list_changed
// whenever AddResource / RemoveResources is called, so the only thing this
// provider needs to do is avoid churning the resource set when nothing actually
// changed (otherwise every refresh emits a spurious list_changed). The
// sortedURIs field tracks the last-applied URI set so doRefresh can short-
// circuit when the new state matches.
type PinnedChatsProvider struct {
	client      *tg.Client
	provider    *messages.Provider
	server      *mcp.Server
	logger      *slog.Logger
	mu          sync.Mutex
	currentURIs []string           // track current pinned resource URIs for cleanup (unsorted, preserves Telegram order)
	sortedURIs  []string           // sorted copy for O(len) diffing via slices.Equal
	sfGroup     singleflight.Group // deduplicates concurrent refresh calls
}

// PinnedChatResource represents a pinned chat resource content.
type PinnedChatResource struct {
	Chat     tgdata.ChatInfo    `json:"chat"`
	Messages []messages.Message `json:"messages"`
}

// NewPinnedChatsProvider creates a new PinnedChatsProvider. The logger is
// used to surface refresh failures from the background watcher — nil falls
// back to slog.Default so this stays safe to call from tests.
func NewPinnedChatsProvider(client *tg.Client, provider *messages.Provider, srv *mcp.Server, logger *slog.Logger) *PinnedChatsProvider {
	if logger == nil {
		logger = slog.Default()
	}
	return &PinnedChatsProvider{
		client:   client,
		provider: provider,
		server:   srv,
		logger:   logger,
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

	// Build the new URI set. sortedNew is compared against the last-applied
	// sorted set via slices.Equal so that Telegram returning the same pinned
	// chats in a different order is a no-op (no spurious list_changed).
	newURIs := make([]string, 0, len(chats))
	for _, chat := range chats {
		newURIs = append(newURIs, fmt.Sprintf("telegram://chats/%d/messages", chat.ID))
	}
	sortedNew := append([]string(nil), newURIs...)
	slices.Sort(sortedNew)

	p.mu.Lock()
	if slices.Equal(sortedNew, p.sortedURIs) {
		// Nothing changed — no need to churn the resource registry or emit a
		// spurious notifications/resources/list_changed. This makes the
		// background ticker safe to run frequently without spamming clients.
		p.mu.Unlock()
		return nil
	}
	prevURIs := p.currentURIs
	p.currentURIs = newURIs
	p.sortedURIs = sortedNew
	p.mu.Unlock()

	// Remove previously added pinned resources.
	if len(prevURIs) > 0 {
		p.server.RemoveResources(prevURIs...)
	}

	// Register the new pinned resources individually. Each closure captures
	// its own chat for the handler.
	for _, chat := range chats {
		uri := fmt.Sprintf("telegram://chats/%d/messages", chat.ID)
		chatCopy := chat
		p.server.AddResource(&mcp.Resource{
			URI:         uri,
			Name:        fmt.Sprintf("Messages from %s", chat.Name),
			Description: fmt.Sprintf("Last 100 messages from chat: %s (%s)", chat.Name, chat.Type),
			MIMEType:    "application/json",
		}, func(ctx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return p.handlePinnedChat(ctx, request, chatCopy)
		})
	}
	// Both AddResource and RemoveResources auto-emit
	// notifications/resources/list_changed, so clients that subscribed to
	// resource updates will re-list and pick up the new set.
	return nil
}

// WatchInBackground starts a goroutine that periodically refreshes the pinned
// chat resource set. Stops when ctx is cancelled. Replaces the on-demand
// BeforeListResources hook from the previous SDK (which has no equivalent in
// the official Go SDK) with a tighter polling interval. Returns a channel
// that closes when the watcher goroutine has fully exited — the caller must
// wait on it before tearing down the MCP server so the goroutine cannot race
// with server shutdown while in the middle of AddResource/RemoveResources.
//
// Refresh errors are logged at Warn (Debug for context cancellation, which
// is not a real error — it just means we're shutting down). Silent failures
// would hide auth/flood-wait problems from the operator indefinitely.
func (p *PinnedChatsProvider) WatchInBackground(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	if interval <= 0 {
		close(done)
		return done
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer close(done)
		defer ticker.Stop()
		if err := p.RefreshResources(ctx); err != nil {
			switch {
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				p.logger.Debug("pinned chats initial refresh cancelled", "err", err)
			default:
				p.logger.Error("pinned chats initial refresh failed", "err", err)
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := p.RefreshResources(ctx); err != nil {
					switch {
					case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
						p.logger.Debug("pinned chats refresh cancelled", "err", err)
					default:
						p.logger.Warn("pinned chats refresh failed", "err", err)
					}
				}
			}
		}
	}()
	return done
}

// handlePinnedChat fetches the last 100 messages for a pinned chat.
func (p *PinnedChatsProvider) handlePinnedChat(
	ctx context.Context,
	request *mcp.ReadResourceRequest,
	chat tgdata.ChatInfo,
) (*mcp.ReadResourceResult, error) {
	opts := messages.FetchOptions{Limit: 100}

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

	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      request.Params.URI,
			MIMEType: "application/json",
			Text:     string(data),
		}},
	}, nil
}
