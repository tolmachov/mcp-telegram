package messages

import (
	"context"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	telegramfake "github.com/tolmachov/mcp-telegram/internal/testutil/telegram"
)

func messageChannelID(id int64) int64 {
	return -1_000_000_000_000 - id
}

func resolveMessageChannelStep(t *testing.T, id, hash int64) telegramfake.InvokeFunc {
	t.Helper()
	return telegramfake.Typed(func(_ context.Context, req *tg.ChannelsGetChannelsRequest, out *tg.MessagesChatsBox) error {
		require.Len(t, req.ID, 1)
		assert.Equal(t, id, req.ID[0].(*tg.InputChannel).ChannelID)
		out.Chats = &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Channel{ID: id, AccessHash: hash, Title: "Channel"}}}
		return nil
	})
}

func TestFetchRefreshesStalePeerExactlyOnce(t *testing.T) {
	const channelID = int64(77)
	inv := telegramfake.New(
		resolveMessageChannelStep(t, channelID, 100),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetHistoryRequest, _ *tg.MessagesMessagesBox) error {
			assert.Equal(t, int64(100), req.Peer.(*tg.InputPeerChannel).AccessHash)
			return tgerr.New(400, "CHANNEL_INVALID")
		}),
		resolveMessageChannelStep(t, channelID, 200),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetHistoryRequest, out *tg.MessagesMessagesBox) error {
			assert.Equal(t, int64(200), req.Peer.(*tg.InputPeerChannel).AccessHash)
			out.Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 9, Date: 100, Message: "fresh"}}}
			return nil
		}),
	)
	p := NewProviderWithRate(tg.NewClient(inv), 100_000)

	got, err := p.Fetch(t.Context(), messageChannelID(channelID), FetchOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "fresh", got.Messages[0].Text)
	assert.Zero(t, inv.Remaining())
}

func TestFetchAllContinuesServiceOnlyPageAndPreservesPartialResult(t *testing.T) {
	const channelID = int64(78)

	t.Run("service-only page advances", func(t *testing.T) {
		inv := telegramfake.New(
			resolveMessageChannelStep(t, channelID, 101),
			telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetHistoryRequest, out *tg.MessagesMessagesBox) error {
				assert.Zero(t, req.OffsetID)
				out.Messages = &tg.MessagesMessagesSlice{Count: 2, Messages: []tg.MessageClass{&tg.MessageService{ID: 10}}}
				return nil
			}),
			telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetHistoryRequest, out *tg.MessagesMessagesBox) error {
				assert.Equal(t, 10, req.OffsetID)
				out.Messages = &tg.MessagesMessagesSlice{Count: 2, Messages: []tg.MessageClass{&tg.Message{ID: 9, Date: 90, Message: "visible"}}}
				return nil
			}),
			telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetHistoryRequest, out *tg.MessagesMessagesBox) error {
				assert.Equal(t, 9, req.OffsetID)
				out.Messages = &tg.MessagesMessagesSlice{Count: 2}
				return nil
			}),
		)
		p := NewProviderWithRate(tg.NewClient(inv), 100_000)
		got, err := p.FetchAll(t.Context(), messageChannelID(channelID), FetchOptions{Limit: 1}, nil)
		require.NoError(t, err)
		require.Len(t, got.Messages, 1)
		assert.Equal(t, 9, got.Messages[0].ID)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("late error returns accumulated messages", func(t *testing.T) {
		inv := telegramfake.New(
			resolveMessageChannelStep(t, channelID, 101),
			telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetHistoryRequest, out *tg.MessagesMessagesBox) error {
				out.Messages = &tg.MessagesMessagesSlice{Count: 2, Messages: []tg.MessageClass{&tg.Message{ID: 9, Date: 90, Message: "kept"}}}
				return nil
			}),
			telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetHistoryRequest, _ *tg.MessagesMessagesBox) error {
				return tgerr.New(500, "INTERNAL")
			}),
		)
		p := NewProviderWithRate(tg.NewClient(inv), 100_000)
		got, err := p.FetchAll(t.Context(), messageChannelID(channelID), FetchOptions{Limit: 1}, nil)
		require.Error(t, err)
		require.NotNil(t, got)
		require.Len(t, got.Messages, 1)
		assert.Equal(t, "kept", got.Messages[0].Text)
	})
}

func TestFetchContextIncludesAnchorWhenAfterZero(t *testing.T) {
	const channelID = int64(79)
	inv := telegramfake.New(
		resolveMessageChannelStep(t, channelID, 102),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetHistoryRequest, out *tg.MessagesMessagesBox) error {
			assert.Equal(t, 20, req.OffsetID)
			assert.Equal(t, -1, req.AddOffset)
			out.Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{
				&tg.Message{ID: 20, Date: 20, Message: "anchor"},
				&tg.Message{ID: 19, Date: 19, Message: "before"},
			}}
			return nil
		}),
	)
	p := NewProviderWithRate(tg.NewClient(inv), 100_000)
	got, err := p.FetchContext(t.Context(), messageChannelID(channelID), 20, 1, 0)
	require.NoError(t, err)
	require.Len(t, got.Messages, 2)
	assert.Equal(t, []int{19, 20}, []int{got.Messages[0].ID, got.Messages[1].ID})
	assert.Zero(t, inv.Remaining())
}

func TestFetchScheduledIsUnpaginated(t *testing.T) {
	const channelID = int64(80)
	inv := telegramfake.New(
		resolveMessageChannelStep(t, channelID, 103),
		telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetScheduledHistoryRequest, out *tg.MessagesMessagesBox) error {
			out.Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 3, Date: 30, Message: "scheduled"}}}
			return nil
		}),
	)
	p := NewProviderWithRate(tg.NewClient(inv), 100_000)
	got, err := p.FetchScheduled(t.Context(), messageChannelID(channelID))
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.False(t, got.HasMore)
	assert.Zero(t, got.NextID)
	assert.Zero(t, inv.Remaining())
}

func TestSearchAndRepliesBuildTelegramRequests(t *testing.T) {
	const channelID = int64(81)
	from := time.Unix(100, 0).UTC()
	to := time.Unix(200, 0).UTC()

	t.Run("search uses inclusive lower and exclusive upper bound", func(t *testing.T) {
		inv := telegramfake.New(
			resolveMessageChannelStep(t, channelID, 104),
			telegramfake.Typed(func(_ context.Context, req *tg.MessagesSearchRequest, out *tg.MessagesMessagesBox) error {
				assert.Equal(t, "needle", req.Q)
				assert.Equal(t, 99, req.MinDate)
				assert.Equal(t, 200, req.MaxDate)
				out.Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 5, Date: 100, Message: "needle"}}}
				return nil
			}),
		)
		p := NewProviderWithRate(tg.NewClient(inv), 100_000)
		got, err := p.Search(t.Context(), messageChannelID(channelID), SearchOptions{Query: "needle", MinDate: from, MaxDate: to})
		require.NoError(t, err)
		require.Len(t, got.Messages, 1)
		assert.Equal(t, messageChannelID(channelID), got.ChatID)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("replies", func(t *testing.T) {
		inv := telegramfake.New(
			resolveMessageChannelStep(t, channelID, 104),
			telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetRepliesRequest, out *tg.MessagesMessagesBox) error {
				assert.Equal(t, 55, req.MsgID)
				assert.Equal(t, 20, req.Limit)
				out.Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 6, Date: 101, Message: "reply"}}}
				return nil
			}),
		)
		p := NewProviderWithRate(tg.NewClient(inv), 100_000)
		got, err := p.FetchReplies(t.Context(), messageChannelID(channelID), 55, FetchOptions{Limit: 20})
		require.NoError(t, err)
		require.Len(t, got.Messages, 1)
		assert.Zero(t, inv.Remaining())
	})
}

func TestSearchGlobalAndForumTopicsBuildTelegramRequests(t *testing.T) {
	t.Run("global search", func(t *testing.T) {
		inv := telegramfake.New(
			telegramfake.Typed(func(_ context.Context, req *tg.MessagesSearchGlobalRequest, out *tg.MessagesMessagesBox) error {
				assert.Equal(t, "needle", req.Q)
				assert.IsType(t, &tg.InputPeerEmpty{}, req.OffsetPeer)
				out.Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{
					&tg.Message{ID: 4, Date: 100, Message: "needle", PeerID: &tg.PeerChannel{ChannelID: 82}},
				}, Chats: []tg.ChatClass{&tg.Channel{ID: 82, AccessHash: 105, Title: "Global"}}}
				return nil
			}),
		)
		p := NewProviderWithRate(tg.NewClient(inv), 100_000)
		got, err := p.SearchGlobal(t.Context(), GlobalSearchOptions{Query: "needle", Limit: 10})
		require.NoError(t, err)
		require.Len(t, got.Messages, 1)
		assert.Equal(t, "Global", got.Messages[0].ChatTitle)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("forum topics", func(t *testing.T) {
		const channelID = int64(83)
		inv := telegramfake.New(
			resolveMessageChannelStep(t, channelID, 106),
			telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetForumTopicsRequest, out *tg.MessagesForumTopics) error {
				query, ok := req.GetQ()
				assert.True(t, ok)
				assert.Equal(t, "release", query)
				assert.Equal(t, 25, req.Limit)
				out.Count = 1
				out.Topics = []tg.ForumTopicClass{&tg.ForumTopic{ID: 7, TopMessage: 70, Date: 100, Title: "Release"}}
				return nil
			}),
		)
		p := NewProviderWithRate(tg.NewClient(inv), 100_000)
		got, err := p.FetchForumTopics(t.Context(), messageChannelID(channelID), "release", 25, 0, 0, 0, 0)
		require.NoError(t, err)
		require.Len(t, got.Topics, 1)
		assert.Equal(t, "Release", got.Topics[0].Title)
		assert.Zero(t, inv.Remaining())
	})
}

func TestProviderPublicValidation(t *testing.T) {
	p := NewProvider(nil)
	_, err := p.Search(t.Context(), 1, SearchOptions{})
	assert.ErrorContains(t, err, "search query is required")
	_, err = p.Search(t.Context(), 1, SearchOptions{
		Query: "x", MinDate: time.Unix(2, 0), MaxDate: time.Unix(1, 0),
	})
	assert.ErrorContains(t, err, "date window is empty")
	_, err = p.SearchGlobal(t.Context(), GlobalSearchOptions{})
	assert.ErrorContains(t, err, "search query is required")
	_, err = p.SearchGlobal(t.Context(), GlobalSearchOptions{
		Query: "x", MinDate: time.Unix(2, 0), MaxDate: time.Unix(1, 0),
	})
	assert.ErrorContains(t, err, "date window is empty")
	_, err = p.FetchReplies(t.Context(), 1, 0, FetchOptions{})
	assert.ErrorContains(t, err, "must be positive")

	// Non-positive configured rates are normalized rather than constructing a
	// limiter that can never make progress.
	assert.NotNil(t, NewProviderWithRate(nil, 0).limiter)
}

func TestFetchUnreadUsesDialogReadBoundary(t *testing.T) {
	const channelID = int64(84)
	inv := telegramfake.New(
		resolveMessageChannelStep(t, channelID, 107),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetPeerDialogsRequest, out *tg.MessagesPeerDialogs) error {
			require.Len(t, req.Peers, 1)
			out.Dialogs = []tg.DialogClass{&tg.Dialog{ReadInboxMaxID: 40}}
			return nil
		}),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetHistoryRequest, out *tg.MessagesMessagesBox) error {
			assert.Equal(t, 40, req.MinID)
			out.Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 41, Date: 100, Message: "unread"}}}
			return nil
		}),
	)
	p := NewProviderWithRate(tg.NewClient(inv), 100_000)
	got, err := p.Fetch(t.Context(), messageChannelID(channelID), FetchOptions{UnreadOnly: true})
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, 41, got.Messages[0].ID)
	assert.Zero(t, inv.Remaining())
}
