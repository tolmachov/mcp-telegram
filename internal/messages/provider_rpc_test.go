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

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

func resolveMessageChannelStep(t *testing.T, id, hash int64) telegramfake.InvokeFunc {
	t.Helper()
	return telegramfake.Typed(func(_ context.Context, req *tg.ChannelsGetChannelsRequest, out *tg.MessagesChatsBox) error {
		require.Len(t, req.ID, 1)
		assert.Equal(t, id, req.ID[0].(*tg.InputChannel).ChannelID)
		out.Chats = &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Channel{ID: id, AccessHash: hash, Title: "Channel"}}}
		return nil
	})
}

// notUserStep answers the resolver's users.getUsers probe with "not a user",
// so resolution falls through to the channel probe.
func notUserStep(t *testing.T, id int64) telegramfake.InvokeFunc {
	t.Helper()
	return telegramfake.Typed(func(_ context.Context, req *tg.UsersGetUsersRequest, out *tg.UserClassVector) error {
		require.Len(t, req.ID, 1)
		assert.Equal(t, id, req.ID[0].(*tg.InputUser).UserID)
		out.Elems = nil
		return nil
	})
}

func TestFetchRefreshesStalePeerExactlyOnce(t *testing.T) {
	const channelID = int64(77)
	inv := telegramfake.New(
		notUserStep(t, channelID),
		resolveMessageChannelStep(t, channelID, 100),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetHistoryRequest, _ *tg.MessagesMessagesBox) error {
			assert.Equal(t, int64(100), req.Peer.(*tg.InputPeerChannel).AccessHash)
			return tgerr.New(400, "CHANNEL_INVALID")
		}),
		notUserStep(t, channelID),
		resolveMessageChannelStep(t, channelID, 200),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetHistoryRequest, out *tg.MessagesMessagesBox) error {
			assert.Equal(t, int64(200), req.Peer.(*tg.InputPeerChannel).AccessHash)
			out.Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 9, Date: 100, Message: "fresh"}}}
			return nil
		}),
	)
	p := NewProvider(tgclient.NewResolver(tg.NewClient(inv), 100_000))

	got, err := p.Fetch(t.Context(), channelID, FetchOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "fresh", got.Messages[0].Text)
	assert.Zero(t, inv.Remaining())
}

func TestFetchAllContinuesServiceOnlyPageAndPreservesPartialResult(t *testing.T) {
	const channelID = int64(78)

	t.Run("service-only page advances", func(t *testing.T) {
		inv := telegramfake.New(
			notUserStep(t, channelID),
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
		p := NewProvider(tgclient.NewResolver(tg.NewClient(inv), 100_000))
		got, err := p.FetchAll(t.Context(), channelID, FetchOptions{Limit: 1}, nil)
		require.NoError(t, err)
		require.Len(t, got.Messages, 1)
		assert.Equal(t, 9, got.Messages[0].ID)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("late error returns accumulated messages", func(t *testing.T) {
		inv := telegramfake.New(
			notUserStep(t, channelID),
			resolveMessageChannelStep(t, channelID, 101),
			telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetHistoryRequest, out *tg.MessagesMessagesBox) error {
				out.Messages = &tg.MessagesMessagesSlice{Count: 2, Messages: []tg.MessageClass{&tg.Message{ID: 9, Date: 90, Message: "kept"}}}
				return nil
			}),
			telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetHistoryRequest, _ *tg.MessagesMessagesBox) error {
				return tgerr.New(500, "INTERNAL")
			}),
		)
		p := NewProvider(tgclient.NewResolver(tg.NewClient(inv), 100_000))
		got, err := p.FetchAll(t.Context(), channelID, FetchOptions{Limit: 1}, nil)
		require.Error(t, err)
		require.NotNil(t, got)
		require.Len(t, got.Messages, 1)
		assert.Equal(t, "kept", got.Messages[0].Text)
	})
}

func TestFetchContextIncludesAnchorWhenAfterZero(t *testing.T) {
	const channelID = int64(79)
	inv := telegramfake.New(
		notUserStep(t, channelID),
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
	p := NewProvider(tgclient.NewResolver(tg.NewClient(inv), 100_000))
	got, err := p.FetchContext(t.Context(), channelID, 20, 1, 0)
	require.NoError(t, err)
	require.Len(t, got.Messages, 2)
	assert.Equal(t, []int{19, 20}, []int{got.Messages[0].ID, got.Messages[1].ID})
	assert.Zero(t, inv.Remaining())
}

func TestFetchScheduledIsUnpaginated(t *testing.T) {
	const channelID = int64(80)
	inv := telegramfake.New(
		notUserStep(t, channelID),
		resolveMessageChannelStep(t, channelID, 103),
		telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetScheduledHistoryRequest, out *tg.MessagesMessagesBox) error {
			out.Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 3, Date: 30, Message: "scheduled"}}}
			return nil
		}),
	)
	p := NewProvider(tgclient.NewResolver(tg.NewClient(inv), 100_000))
	got, err := p.FetchScheduled(t.Context(), channelID)
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
			notUserStep(t, channelID),
			resolveMessageChannelStep(t, channelID, 104),
			telegramfake.Typed(func(_ context.Context, req *tg.MessagesSearchRequest, out *tg.MessagesMessagesBox) error {
				assert.Equal(t, "needle", req.Q)
				assert.Equal(t, 99, req.MinDate)
				assert.Equal(t, 200, req.MaxDate)
				out.Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 5, Date: 100, Message: "needle"}}}
				return nil
			}),
		)
		p := NewProvider(tgclient.NewResolver(tg.NewClient(inv), 100_000))
		got, err := p.Search(t.Context(), channelID, SearchOptions{Query: "needle", MinDate: from, MaxDate: to})
		require.NoError(t, err)
		require.Len(t, got.Messages, 1)
		assert.Equal(t, channelID, got.ChatID)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("replies", func(t *testing.T) {
		inv := telegramfake.New(
			notUserStep(t, channelID),
			resolveMessageChannelStep(t, channelID, 104),
			telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetRepliesRequest, out *tg.MessagesMessagesBox) error {
				assert.Equal(t, 55, req.MsgID)
				assert.Equal(t, 20, req.Limit)
				out.Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 6, Date: 101, Message: "reply"}}}
				return nil
			}),
		)
		p := NewProvider(tgclient.NewResolver(tg.NewClient(inv), 100_000))
		got, err := p.FetchReplies(t.Context(), channelID, 55, FetchOptions{Limit: 20})
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
		p := NewProvider(tgclient.NewResolver(tg.NewClient(inv), 100_000))
		got, err := p.SearchGlobal(t.Context(), GlobalSearchOptions{Query: "needle", Limit: 10})
		require.NoError(t, err)
		require.Len(t, got.Messages, 1)
		assert.Equal(t, "Global", got.Messages[0].ChatTitle)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("forum topics", func(t *testing.T) {
		const channelID = int64(83)
		inv := telegramfake.New(
			notUserStep(t, channelID),
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
		p := NewProvider(tgclient.NewResolver(tg.NewClient(inv), 100_000))
		got, err := p.FetchForumTopics(t.Context(), channelID, "release", 25, 0, 0, 0, 0)
		require.NoError(t, err)
		require.Len(t, got.Topics, 1)
		assert.Equal(t, "Release", got.Topics[0].Title)
		assert.Zero(t, inv.Remaining())
	})
}

func TestProviderPublicValidation(t *testing.T) {
	p := NewProvider(tgclient.NewResolver(nil, 1))
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
}

func TestFetchUnreadUsesDialogReadBoundary(t *testing.T) {
	const channelID = int64(84)
	inv := telegramfake.New(
		notUserStep(t, channelID),
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
	p := NewProvider(tgclient.NewResolver(tg.NewClient(inv), 100_000))
	got, err := p.Fetch(t.Context(), channelID, FetchOptions{UnreadOnly: true})
	require.NoError(t, err)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, 41, got.Messages[0].ID)
	assert.Zero(t, inv.Remaining())
}

func TestForumPaginationCountsOnlyThroughLiveAnchor(t *testing.T) {
	const channelID = int64(85)
	inv := telegramfake.New(
		notUserStep(t, channelID),
		resolveMessageChannelStep(t, channelID, 108),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetForumTopicsRequest, out *tg.MessagesForumTopics) error {
			assert.Zero(t, req.OffsetTopic)
			assert.Equal(t, 100, req.Limit) // default limit
			out.Count = 4
			out.Topics = []tg.ForumTopicClass{&tg.ForumTopic{ID: 7, TopMessage: 70, Date: 10}, &tg.ForumTopicDeleted{ID: 8}}
			out.Messages = []tg.MessageClass{&tg.Message{ID: 70, Date: 100}}
			return nil
		}),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetForumTopicsRequest, out *tg.MessagesForumTopics) error {
			assert.Equal(t, 7, req.OffsetTopic)
			assert.Equal(t, 70, req.OffsetID)
			assert.Equal(t, 100, req.OffsetDate)
			out.Count = 4
			out.Topics = []tg.ForumTopicClass{&tg.ForumTopicDeleted{ID: 8}, &tg.ForumTopic{ID: 9, TopMessage: 90, Date: 9}}
			out.Messages = []tg.MessageClass{&tg.Message{ID: 90, Date: 90}}
			return nil
		}),
		telegramfake.Typed(func(_ context.Context, req *tg.MessagesGetForumTopicsRequest, out *tg.MessagesForumTopics) error {
			assert.Equal(t, 9, req.OffsetTopic)
			assert.Equal(t, 90, req.OffsetID)
			assert.Equal(t, 90, req.OffsetDate)
			out.Count = 4
			out.Topics = []tg.ForumTopicClass{&tg.ForumTopic{ID: 10, TopMessage: 100, Date: 8}}
			return nil
		}),
	)
	p := NewProvider(tgclient.NewResolver(tg.NewClient(inv), 100_000))
	offset := ForumTopicsOffset{}
	var ids []int
	for range 3 {
		got, err := p.FetchForumTopics(t.Context(), channelID, "", 0, offset.Topic, offset.ID, offset.Date, offset.Seen)
		require.NoError(t, err)
		for _, topic := range got.Topics {
			ids = append(ids, topic.ID)
		}
		if got.NextOffset == nil {
			break
		}
		offset = *got.NextOffset
	}
	assert.Equal(t, []int{7, 9, 10}, ids)
	assert.Zero(t, inv.Remaining(), "trailing tombstones must not truncate the next page")
}

func TestForumPaginationRejectsRepeatedAnchorEvenAtReportedEnd(t *testing.T) {
	inv := telegramfake.New(
		notUserStep(t, 85),
		resolveMessageChannelStep(t, 85, 108),
		telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetForumTopicsRequest, out *tg.MessagesForumTopics) error {
			out.Count = 2
			out.Topics = []tg.ForumTopicClass{&tg.ForumTopic{ID: 7, TopMessage: 70, Date: 10}, &tg.ForumTopicDeleted{ID: 8}}
			return nil
		}),
	)
	p := NewProvider(tgclient.NewResolver(tg.NewClient(inv), 100_000))
	got, err := p.FetchForumTopics(t.Context(), 85, "", 2, 7, 70, 100, 1)
	require.ErrorContains(t, err, "did not advance")
	assert.Nil(t, got)
	assert.Zero(t, inv.Remaining())
}

func TestFetchPreservesMediaAndMetadataForBackup(t *testing.T) {
	photo := &tg.MessageMediaPhoto{}
	photo.SetPhoto(&tg.Photo{ID: 101, AccessHash: 202, DCID: 2, FileReference: []byte("ref"), Sizes: []tg.PhotoSizeClass{
		&tg.PhotoSize{Type: "s", W: 100, H: 80},
		&tg.PhotoSizeProgressive{Type: "x", W: 1280, H: 960},
		&tg.PhotoCachedSize{Type: "m", W: 320, H: 240},
		&tg.PhotoSizeEmpty{Type: "i"},
	}})
	doc := &tg.MessageMediaDocument{}
	doc.SetDocument(&tg.Document{Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "report.pdf"}}})
	picture := &tg.Message{ID: 3, Date: 100, FromID: &tg.PeerUser{UserID: 42}, Media: photo, ReplyTo: &tg.MessageReplyHeader{ReplyToMsgID: 1}}
	picture.SetReactions(tg.MessageReactions{Results: []tg.ReactionCount{
		{Reaction: &tg.ReactionEmoji{Emoticon: "👍"}, Count: 2},
		{Reaction: &tg.ReactionCustomEmoji{DocumentID: 77}, Count: 1},
		{Reaction: &tg.ReactionPaid{}, Count: 3},
	}})
	inv := telegramfake.New(
		notUserStep(t, 86),
		resolveMessageChannelStep(t, 86, 109),
		telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetHistoryRequest, out *tg.MessagesMessagesBox) error {
			out.Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{
				picture,
				&tg.Message{ID: 2, Date: 90, Media: doc},
				&tg.Message{ID: 1, Date: 80, Message: "😀 https://example.com link", Entities: []tg.MessageEntityClass{
					&tg.MessageEntityURL{Offset: 3, Length: 19},
					&tg.MessageEntityTextURL{Offset: 23, Length: 4, URL: "https://hidden.example"},
				}},
			}, Users: []tg.UserClass{&tg.User{ID: 42, FirstName: "Alice"}}}
			return nil
		}),
	)
	p := NewProvider(tgclient.NewResolver(tg.NewClient(inv), 100_000))
	got, err := p.Fetch(t.Context(), 86, DefaultFetchOptions())
	require.NoError(t, err)
	require.Len(t, got.Messages, 3)
	assert.Equal(t, &MediaInfo{Type: "photo", Width: 1280, Height: 960, ResourceURI: "telegram://media/101/202/2/x?ref=cmVm"}, got.Messages[0].Media)
	assert.Equal(t, "Alice", got.Messages[0].SenderName)
	assert.Equal(t, 1, got.Messages[0].ReplyToID)
	assert.Equal(t, &MediaInfo{Type: "document", FileName: "report.pdf"}, got.Messages[1].Media)
	assert.Equal(t, []string{"https://example.com", "https://hidden.example"}, got.Messages[2].Entities)
	backup := FormatBatchForBackup(got.Messages)
	assert.Contains(t, backup, "[Alice] [id=3] [reply_to=1] [reactions: 👍x2, custom:77x1, ⭐x3]")
	assert.Contains(t, backup, "[media: photo, resource=telegram://media/101/202/2/x?ref=cmVm, size=1280x960]")
	assert.Contains(t, backup, "[media: document, file=report.pdf]")
	assert.Zero(t, inv.Remaining())
}
