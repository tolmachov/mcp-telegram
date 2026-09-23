package messages

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// withPeerRetry executes an operation with a cached peer, then invalidates all
// involved access hashes and performs exactly one fresh attempt when Telegram
// reports a stale peer. keep, when non-nil, reports whether a failed attempt's
// result holds progress a retry would throw away; such a result is returned
// with its error instead of being retried.
func withPeerRetry[T any](ctx context.Context, p *Provider, chatID int64, related []int64, keep func(T) bool, operation func(tg.InputPeerClass) (T, error)) (T, error) {
	var zero T
	peer, err := p.peers.Resolve(ctx, p.client, chatID)
	if err != nil {
		return zero, fmt.Errorf("resolving peer: %w", err)
	}
	result, err := operation(peer)
	if err == nil || !tgclient.ShouldRefreshPeer(err) || (keep != nil && keep(result)) {
		return result, err
	}
	p.peers.Invalidate(chatID)
	for _, id := range related {
		p.peers.Invalidate(id)
	}
	peer, err = p.peers.Resolve(ctx, p.client, chatID)
	if err != nil {
		return zero, fmt.Errorf("freshly resolving peer: %w", err)
	}
	return operation(peer)
}
