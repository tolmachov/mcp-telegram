package messages

import (
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	got := buildForumTopicsResult(resp, 4)

	require.Len(t, got.Topics, 3)
	assert.Equal(t, 50, got.Count)

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

	got := buildForumTopicsResult(resp, 100)

	require.Len(t, got.Topics, 2)
	assert.Nil(t, got.NextOffset)
}

// TestBuildForumTopicsResultAllDeleted locks in the fix for the pagination
// loop: a full page consisting only of deleted topics (with Count reporting
// more) must NOT advertise a next page, because there is no real topic to
// anchor an advancing offset on. NextOffset stays nil so the tool emits no
// (zero) cursor that would restart from the first page forever.
func TestBuildForumTopicsResultAllDeleted(t *testing.T) {
	resp := &tg.MessagesForumTopics{
		Count: 50,
		Topics: []tg.ForumTopicClass{
			&tg.ForumTopicDeleted{ID: 1},
			&tg.ForumTopicDeleted{ID: 2},
		},
	}

	got := buildForumTopicsResult(resp, 2)

	assert.Empty(t, got.Topics)
	assert.Nil(t, got.NextOffset)
}

// TestBuildForumTopicsResultDateFallback exercises the offset_date fallback:
// when the last topic's top message is absent from resp.Messages, the offset
// date falls back to the topic's own creation date.
func TestBuildForumTopicsResultDateFallback(t *testing.T) {
	resp := &tg.MessagesForumTopics{
		Count: 50,
		Topics: []tg.ForumTopicClass{
			&tg.ForumTopic{ID: 3, Title: "A", TopMessage: 30, Date: 111},
			&tg.ForumTopic{ID: 4, Title: "B", TopMessage: 40, Date: 222},
		},
		// no Messages → top message 40 not found → fallback to lastTopic.Date
	}

	got := buildForumTopicsResult(resp, 2)

	require.NotNil(t, got.NextOffset)
	assert.Equal(t, 4, got.NextOffset.Topic)
	assert.Equal(t, 40, got.NextOffset.ID)
	assert.Equal(t, 222, got.NextOffset.Date) // lastTopic.Date fallback
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
	p := NewProvider(nil)

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
