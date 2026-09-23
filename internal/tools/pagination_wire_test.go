package tools

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/presentation"
	telegramfake "github.com/tolmachov/mcp-telegram/internal/testutil/telegram"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

func TestPaginationThroughMCP(t *testing.T) {
	const chatID = int64(77)
	for _, name := range []string{"GetMessages", "SearchMessages", "GetReplies", "GetForumTopics"} {
		t.Run(name, func(t *testing.T) {
			firstArgs := map[string]any{"chat_id": chatID, "limit": 2}
			if name == "GetForumTopics" {
				firstArgs["query"] = "release"
			} else {
				firstArgs["from_date"] = "1970-01-01T00:01:40Z"
				firstArgs["to_date"] = "1970-01-01T00:03:20Z"
			}
			if name == "SearchMessages" {
				firstArgs["query"] = "release"
				firstArgs["media_type"] = "photos"
				firstArgs["top_msg_id"] = presentation.FormatRegularRef(7)
				firstArgs["from_sender_id"] = chatID
			}
			if name == "GetReplies" {
				firstArgs["message_id"] = presentation.FormatRegularRef(7)
			}

			page := func(next bool) telegramfake.InvokeFunc {
				return func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
					offset := 0
					if next {
						offset = 20
					}
					switch req := input.(type) {
					case *tg.MessagesGetHistoryRequest:
						assert.Equal(t, "GetMessages", name)
						assert.Equal(t, offset, req.OffsetID)
						assert.Equal(t, 200, req.OffsetDate)
						assert.Equal(t, 2, req.Limit)
						assert.Equal(t, int64(77), req.Peer.(*tg.InputPeerChannel).ChannelID)
					case *tg.MessagesSearchRequest:
						assert.Equal(t, "SearchMessages", name)
						assert.Equal(t, offset, req.OffsetID)
						assert.Equal(t, 99, req.MinDate)
						assert.Equal(t, 200, req.MaxDate)
						assert.Equal(t, 2, req.Limit)
						assert.Equal(t, "release", req.Q)
						assert.Equal(t, 7, req.TopMsgID)
						assert.Equal(t, &tg.InputPeerChannel{ChannelID: 77, AccessHash: 100}, req.FromID)
						assert.IsType(t, &tg.InputMessagesFilterPhotos{}, req.Filter)
					case *tg.MessagesGetRepliesRequest:
						assert.Equal(t, "GetReplies", name)
						assert.Equal(t, offset, req.OffsetID)
						assert.Equal(t, 200, req.OffsetDate)
						assert.Equal(t, 2, req.Limit)
						assert.Equal(t, 7, req.MsgID)
					case *tg.MessagesGetForumTopicsRequest:
						assert.Equal(t, "GetForumTopics", name)
						assert.Equal(t, "release", req.Q)
						assert.Equal(t, 2, req.Limit)
						if next {
							// The first page ends in a tombstone: all offsets still belong to topic 7.
							assert.Equal(t, 7, req.OffsetTopic)
							assert.Equal(t, 20, req.OffsetID)
							assert.Equal(t, 150, req.OffsetDate)
						} else {
							assert.Zero(t, req.OffsetTopic)
							assert.Zero(t, req.OffsetID)
							assert.Zero(t, req.OffsetDate)
						}
					default:
						return assert.AnError
					}
					switch out := output.(type) {
					case *tg.MessagesMessagesBox:
						if next {
							out.Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 19, Date: 120, Message: "second"}}}
						} else {
							out.Messages = &tg.MessagesMessagesSlice{Count: 3, Messages: []tg.MessageClass{&tg.Message{ID: 20, Date: 150, Message: "first"}}}
						}
					case *tg.MessagesForumTopics:
						out.Count = 3
						if next {
							out.Topics = []tg.ForumTopicClass{&tg.ForumTopicDeleted{ID: 8}, &tg.ForumTopic{ID: 9, TopMessage: 19, Date: 120, Title: "second"}}
						} else {
							out.Topics = []tg.ForumTopicClass{&tg.ForumTopic{ID: 7, TopMessage: 20, Date: 110, Title: "first"}, &tg.ForumTopicDeleted{ID: 8}}
							out.Messages = []tg.MessageClass{&tg.Message{ID: 20, Date: 150}}
						}
					default:
						return assert.AnError
					}
					return nil
				}
			}
			inv := telegramfake.New(
				notUserStep(t, chatID),
				telegramfake.Typed(func(_ context.Context, _ *tg.ChannelsGetChannelsRequest, out *tg.MessagesChatsBox) error {
					out.Chats = &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Channel{ID: 77, AccessHash: 100}}}
					return nil
				}), page(false), page(true),
			)
			provider := messages.NewProvider(tgclient.NewResolver(tg.NewClient(inv), 100_000))
			cs := connectToolClient(t, func(s *mcp.Server) {
				switch name {
				case "GetMessages":
					NewMessagesGetHandler(provider).Register(s)
				case "SearchMessages":
					NewMessagesSearchHandler(provider).Register(s)
				case "GetReplies":
					NewGetRepliesHandler(provider).Register(s)
				case "GetForumTopics":
					NewGetForumTopicsHandler(provider).Register(s)
				}
			})
			call := func(args map[string]any) *mcp.CallToolResult {
				t.Helper()
				res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
				require.NoError(t, err)
				return res
			}
			// Making first-page fields optional in JSON Schema must not make them optional in the handler.
			require.True(t, call(map[string]any{}).IsError)
			if name == "SearchMessages" || name == "GetReplies" {
				require.True(t, call(map[string]any{"chat_id": chatID}).IsError)
			}
			first := call(firstArgs)
			require.False(t, first.IsError, "%+v", first.Content)
			body := structured(t, first)
			require.Equal(t, true, body["has_more"])
			cursor, ok := body["next_cursor"].(string)
			require.True(t, ok)
			require.NotEmpty(t, cursor)
			require.True(t, call(map[string]any{"cursor": cursor, "chat_id": chatID}).IsError)
			second := call(map[string]any{"cursor": cursor})
			require.False(t, second.IsError, "%+v", second.Content)
			body = structured(t, second)
			assert.Equal(t, false, body["has_more"])
			assert.Equal(t, float64(1), body["count"])
			assert.Equal(t, float64(chatID), body["chat_id"])
			field, wantID := "messages", presentation.FormatRegularRef(19)
			if name == "GetForumTopics" {
				field, wantID = "topics", presentation.FormatRegularRef(9)
			}
			items := body[field].([]any)
			require.Len(t, items, 1)
			assert.Equal(t, wantID, items[0].(map[string]any)["id"])
			assert.Zero(t, inv.Remaining())
		})
	}
}
