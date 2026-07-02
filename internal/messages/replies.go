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

	peer, err := p.peers.Resolve(ctx, p.client, chatID)
	if err != nil {
		return nil, fmt.Errorf("resolving peer: %w", err)
	}

	req := &tg.MessagesGetRepliesRequest{
		Peer:     peer,
		MsgID:    rootMsgID,
		Limit:    opts.Limit,
		OffsetID: opts.OffsetID,
	}
	if offsetDate := historyOffsetDate(opts); !offsetDate.IsZero() {
		req.OffsetDate = int(offsetDate.Unix())
	}

	p.limiter.Take()

	history, err := p.client.MessagesGetReplies(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("getting replies: %w", err)
	}

	result, err := p.processHistory(history, peer, opts.Limit)
	if err != nil {
		return nil, err
	}

	applyMinDateFilter(result, opts.MinDate)
	result.ChatID = chatID
	return result, nil
}
