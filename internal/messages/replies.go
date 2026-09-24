package messages

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
)

// FetchReplies retrieves the messages of a reply thread via
// messages.getReplies. The same call backs two user-facing cases that share
// identical mechanics:
//
//   - Comments under a channel post: chatID is the channel and rootMsgID is
//     the post ID. Telegram resolves the linked discussion group internally.
//   - Messages inside a forum-supergroup topic: chatID is the supergroup and
//     rootMsgID is the topic ID (as returned by FetchForumTopics).
//
// Pagination mirrors Fetch: pass the previous result's NextID as OffsetID.
// MaxDate maps to Telegram's native offset_date (strictly-less-than); MinDate
// has no native equivalent and is applied as the same post-filter Fetch uses.
func (p *Provider) FetchReplies(ctx context.Context, chatID int64, rootMsgID int, opts FetchOptions) (*FetchResult, error) {
	if rootMsgID <= 0 {
		return nil, fmt.Errorf("root message id must be positive, got %d", rootMsgID)
	}
	if opts.Limit <= 0 {
		opts.Limit = 50
	}

	return p.forChat(ctx, chatID, func(peer tg.InputPeerClass) (*FetchResult, error) {
		return p.fetchRepliesWithPeer(ctx, peer, rootMsgID, opts)
	})
}

func (p *Provider) fetchRepliesWithPeer(ctx context.Context, peer tg.InputPeerClass, rootMsgID int, opts FetchOptions) (*FetchResult, error) {

	req := &tg.MessagesGetRepliesRequest{
		Peer:     peer,
		MsgID:    rootMsgID,
		Limit:    opts.Limit,
		OffsetID: opts.OffsetID,
	}
	if offsetDate := historyOffsetDate(opts); !offsetDate.IsZero() {
		req.OffsetDate = telegramBefore(offsetDate)
	}

	if err := p.wait(ctx); err != nil {
		return nil, err
	}

	history, err := p.peers.Client().MessagesGetReplies(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("getting replies: %w", err)
	}

	result := p.processHistory(history, peer)

	applyMinDateFilter(result, opts.MinDate)
	return result, nil
}
