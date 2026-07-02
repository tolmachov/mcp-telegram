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
// sortedFingerprints field tracks the last-applied state so doRefresh can
// short-circuit when the new state matches.
type PinnedChatsProvider struct {
	client   *tg.Client
	provider *messages.Provider
	// servers are the MCP servers the pinned resource set is mirrored onto. With
	// SEP-2053 variants there is one inner server per variant, all fed by this
	// single poller so every variant can list/read pinned chats without
	// multiplying Telegram polling.
	servers []*mcp.Server
	logger  *slog.Logger
	// mu guards currentURIs/sortedFingerprints. doRefresh commits them only after
	// the server registry has been updated, so a reader holding mu sees fields
	// that match what is actually registered (never an interim "about to apply"
	// state). Refreshes are additionally serialized by the singleflight, so mu is
	// really just future-proofing for a second reader.
	mu          sync.Mutex
	currentURIs []string // track current pinned resource URIs for cleanup (unsorted, preserves Telegram order)
	// sortedFingerprints is a sorted per-chat fingerprint (uri + name + type) of
	// the last-applied set, for O(len) diffing via slices.Equal. It keys on more
	// than the URI so that a rename (same chat ID/URI, new title) still counts as
	// a change and re-registers the resource with the fresh Name/Description.
	sortedFingerprints []string
	sfGroup            singleflight.Group // deduplicates concurrent refresh calls
}

// PinnedChatResource represents a pinned chat resource content.
type PinnedChatResource struct {
	Chat     tgdata.ChatInfo    `json:"chat"`
	Messages []messages.Message `json:"messages"`
}

// NewPinnedChatsProvider creates a new PinnedChatsProvider. The logger is
// used to surface refresh failures from the background watcher — nil falls
// back to slog.Default so this stays safe to call from tests.
//
// servers is the set the pinned resources are mirrored onto; passing none is a
// programming error (RefreshResources would fetch from Telegram and register the
// results onto nobody), so it is logged loudly rather than silently no-ooping.
func NewPinnedChatsProvider(client *tg.Client, provider *messages.Provider, logger *slog.Logger, servers ...*mcp.Server) *PinnedChatsProvider {
	if logger == nil {
		logger = slog.Default()
	}
	if len(servers) == 0 {
		logger.Warn("pinned-chat provider created with no servers; pinned resources will be fetched but registered nowhere")
	}
	return &PinnedChatsProvider{
		client:   client,
		provider: provider,
		servers:  servers,
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

	// Build the new URI set plus a per-chat fingerprint. sortedNew is compared
	// against the last-applied sorted set via slices.Equal so that Telegram
	// returning the same pinned chats in a different order is a no-op (no
	// spurious list_changed). The fingerprint includes the chat name and type,
	// not just the URI, so a renamed pinned chat (same ID/URI) still re-registers
	// with its fresh Name/Description instead of showing a stale title forever.
	newURIs := make([]string, 0, len(chats))
	sortedNew := make([]string, 0, len(chats))
	for _, chat := range chats {
		uri := fmt.Sprintf("telegram://chats/%d/messages", chat.ID)
		newURIs = append(newURIs, uri)
		// \x00 cannot occur in a URI, chat name, or type string, so it is an
		// unambiguous field separator for the fingerprint.
		sortedNew = append(sortedNew, fmt.Sprintf("%s\x00%s\x00%s", uri, chat.Name, chat.Type))
	}
	slices.Sort(sortedNew)

	p.mu.Lock()
	if slices.Equal(sortedNew, p.sortedFingerprints) {
		// Nothing changed — no need to churn the resource registry or emit a
		// spurious notifications/resources/list_changed. This makes the
		// background ticker safe to run frequently without spamming clients.
		p.mu.Unlock()
		return nil
	}
	prevURIs := p.currentURIs
	p.mu.Unlock()

	// Apply the change to the registry BEFORE committing the state fields below,
	// so currentURIs/sortedFingerprints never claim "applied" for a set that is
	// not actually registered. doRefresh is serialized against itself by the
	// singleflight in RefreshResources, so no concurrent refresh can observe the
	// interim state; the only reader of these fields is doRefresh itself.

	// Remove previously added pinned resources from every mirrored server.
	if len(prevURIs) > 0 {
		for _, srv := range p.servers {
			srv.RemoveResources(prevURIs...)
		}
	}

	// Register the new pinned resources individually on every mirrored server.
	// Each closure captures its own chat for the handler.
	for _, chat := range chats {
		uri := fmt.Sprintf("telegram://chats/%d/messages", chat.ID)
		chatCopy := chat
		res := &mcp.Resource{
			URI:         uri,
			Name:        fmt.Sprintf("Messages from %s", chat.Name),
			Description: fmt.Sprintf("Last 100 messages from chat: %s (%s)", chat.Name, chat.Type),
			MIMEType:    "application/json",
		}
		handler := func(ctx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return p.handlePinnedChat(ctx, request, chatCopy)
		}
		for _, srv := range p.servers {
			srv.AddResource(res, handler)
		}
	}

	// Commit the applied set only now that the registry reflects it.
	p.mu.Lock()
	p.currentURIs = newURIs
	p.sortedFingerprints = sortedNew
	p.mu.Unlock()
	// Both AddResource and RemoveResources auto-emit
	// notifications/resources/list_changed. In single --variant mode that
	// reaches the client directly, so listChanged-capable clients re-list and
	// pick up the new set. In multi-variant mode the variants proxy drops these
	// async notifications (they fire on a background context with no front
	// session — see runHappy and the README), so those clients instead pick up
	// the change on their next client-initiated resources/list.
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
// Refresh errors are logged so an operator sees auth/flood-wait problems
// instead of a silently frozen resource set: the initial refresh failing is
// logged at Error (startup is broken from the first tick), while subsequent
// periodic failures are logged at Warn (they may be transient and the ticker
// retries every interval). Context cancellation is logged at Debug in both
// cases — it is not a real error, just shutdown.
func (p *PinnedChatsProvider) WatchInBackground(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	// Split the two non-positive cases so a malformed config is loud while an
	// intentional disable stays quiet. interval == 0 is the documented "disable
	// the watcher" value (--pinned-refresh-seconds 0), so it only logs at Debug.
	// A negative interval can only be an operator typo (e.g.
	// --pinned-refresh-seconds=-30); silently disabling would leave them
	// debugging why pinned resources never appear, so it warns. Either way we
	// must return before time.NewTicker, which panics on a non-positive interval.
	switch {
	case interval == 0:
		p.logger.Debug("pinned-chat watcher disabled (refresh interval 0)")
		close(done)
		return done
	case interval < 0:
		p.logger.Warn("pinned-chat refresh interval is negative; disabling watcher — check --pinned-refresh-seconds", "interval", interval)
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
