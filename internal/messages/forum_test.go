package messages

import (
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

func TestBuildForumTopicsResult(t *testing.T) {
	general := &tg.ForumTopic{ID: 1, Title: "General", TopMessage: 500, Hidden: true, IconColor: 0x6FB9F0, Date: 100}
	topicWithEmoji := &tg.ForumTopic{ID: 7, Title: "Releases", TopMessage: 510, UnreadCount: 3, Pinned: true, Date: 200}
	topicWithEmoji.SetIconEmojiID(998877)
	closed := &tg.ForumTopic{ID: 9, Title: "Archive", TopMessage: 520, Closed: true, Date: 300}

	resp := &tg.MessagesForumTopics{
		Count: 50, // more than returned → has_more when page is full
		Topics: []tg.ForumTopicClass{
			general,
			topicWithEmoji,
			&tg.ForumTopicDeleted{ID: 8}, // must be skipped
			closed,
		},
		Messages: []tg.MessageClass{
			&tg.Message{ID: 520, Date: 1717000000}, // top message of last topic, for offset_date
		},
	}

	got, err := buildForumTopicsResult(resp, 0)
	require.NoError(t, err)

	require.Len(t, got.Topics, 3)
	assert.Equal(t, 3, got.Count)
	assert.Equal(t, 50, got.Total)
	assert.Equal(t, 4, got.RawCount)

	assert.Equal(t, "General", got.Topics[0].Title)
	assert.True(t, got.Topics[0].Hidden)
	assert.Empty(t, got.Topics[0].IconEmojiID)

	assert.Equal(t, "Releases", got.Topics[1].Title)
	assert.Equal(t, "998877", got.Topics[1].IconEmojiID)
	assert.True(t, got.Topics[1].Pinned)
	assert.Equal(t, 3, got.Topics[1].UnreadCount)

	assert.Equal(t, "Archive", got.Topics[2].Title)
	assert.True(t, got.Topics[2].Closed)

	// Page of 4 raw topics == limit and 3 < Count(50) → more pages.
	require.NotNil(t, got.NextOffset)
	assert.Equal(t, 9, got.NextOffset.Topic)         // last topic ID
	assert.Equal(t, 520, got.NextOffset.ID)          // last topic's TopMessage
	assert.Equal(t, 1717000000, got.NextOffset.Date) // from matching message
}

func TestBuildForumTopicsResultLastPage(t *testing.T) {
	resp := &tg.MessagesForumTopics{
		Count: 2,
		Topics: []tg.ForumTopicClass{
			&tg.ForumTopic{ID: 1, Title: "General", TopMessage: 10, Date: 1},
			&tg.ForumTopic{ID: 2, Title: "Off-topic", TopMessage: 20, Date: 2},
		},
	}

	got, err := buildForumTopicsResult(resp, 0)
	require.NoError(t, err)

	require.Len(t, got.Topics, 2)
	assert.Nil(t, got.NextOffset)
}

func TestBuildForumTopicsPaginationAnchors(t *testing.T) {
	for _, tc := range []struct {
		name        string
		topics      []tg.ForumTopicClass
		messages    []tg.MessageClass
		createOrder bool
		seen        int
		total       int
		want        *ForumTopicsOffset
		wantErr     string
	}{
		{
			name:     "trailing deleted topic resumes from live anchor",
			topics:   []tg.ForumTopicClass{&tg.ForumTopic{ID: 3, TopMessage: 30, Date: 111}, &tg.ForumTopicDeleted{ID: 4}},
			messages: []tg.MessageClass{&tg.Message{ID: 30, Date: 222}},
			seen:     5, total: 10, want: &ForumTopicsOffset{Topic: 3, ID: 30, Date: 222, Seen: 6},
		},
		{
			name:     "service message anchors a newly created topic",
			topics:   []tg.ForumTopicClass{&tg.ForumTopic{ID: 3, TopMessage: 3, Date: 111}},
			messages: []tg.MessageClass{&tg.MessageService{ID: 3, Date: 222}},
			total:    10, want: &ForumTopicsOffset{Topic: 3, ID: 3, Date: 222, Seen: 1},
		},
		{
			name:        "creation ordering uses topic date",
			topics:      []tg.ForumTopicClass{&tg.ForumTopic{ID: 3, TopMessage: 30, Date: 111}},
			messages:    []tg.MessageClass{&tg.Message{ID: 30, Date: 222}},
			createOrder: true, total: 10, want: &ForumTopicsOffset{Topic: 3, ID: 30, Date: 111, Seen: 1},
		},
		{
			name:   "all deleted cannot supply a continuation anchor",
			topics: []tg.ForumTopicClass{&tg.ForumTopicDeleted{ID: 3}, &tg.ForumTopicDeleted{ID: 4}},
			total:  10, wantErr: "only deleted topics",
		},
		{
			name:   "missing top message cannot approximate activity date",
			topics: []tg.ForumTopicClass{&tg.ForumTopic{ID: 3, TopMessage: 30, Date: 111}},
			total:  10, wantErr: "missing anchor data",
		},
		{
			name:   "terminal deleted page needs no anchor",
			topics: []tg.ForumTopicClass{&tg.ForumTopicDeleted{ID: 3}},
			seen:   9, total: 10,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildForumTopicsResult(&tg.MessagesForumTopics{
				Count: tc.total, Topics: tc.topics, Messages: tc.messages, OrderByCreateDate: tc.createOrder,
			}, tc.seen)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.NextOffset)
		})
	}
}

func TestExtractReplies(t *testing.T) {
	t.Run("channel post comments", func(t *testing.T) {
		r := tg.MessageReplies{Comments: true, Replies: 42}
		r.SetChannelID(123456)
		r.SetMaxID(999)

		got := extractReplies(r)
		require.NotNil(t, got)
		assert.Equal(t, 42, got.Count)
		assert.True(t, got.IsComments)
		assert.Equal(t, int64(123456), got.ChannelID)
		assert.Equal(t, 999, got.MaxID)
	})

	t.Run("plain group thread without channel", func(t *testing.T) {
		r := tg.MessageReplies{Comments: false, Replies: 5}
		got := extractReplies(r)
		require.NotNil(t, got)
		assert.Equal(t, 5, got.Count)
		assert.False(t, got.IsComments)
		assert.Zero(t, got.ChannelID)
		assert.Zero(t, got.MaxID)
	})
}

func TestExtractMessagesPopulatesReplies(t *testing.T) {
	p := NewProvider(tgclient.NewResolver(nil, 1))

	withReplies := &tg.Message{ID: 1, Message: "post"}
	replies := tg.MessageReplies{Comments: true, Replies: 7}
	withReplies.SetReplies(replies)

	plain := &tg.Message{ID: 2, Message: "no thread"}

	got := p.extractMessages([]tg.MessageClass{withReplies, plain}, nil, nil, &tg.InputPeerEmpty{})
	require.Len(t, got, 2)

	require.NotNil(t, got[0].Replies)
	assert.Equal(t, 7, got[0].Replies.Count)
	assert.True(t, got[0].Replies.IsComments)

	assert.Nil(t, got[1].Replies)
}
