package tools

import (
	"context"
	"math"
	"strconv"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	telegramfake "github.com/tolmachov/mcp-telegram/internal/testutil/telegram"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

func resolveChannelStep(t *testing.T, id, accessHash int64) telegramfake.InvokeFunc {
	t.Helper()
	return telegramfake.Typed(func(_ context.Context, req *tg.ChannelsGetChannelsRequest, out *tg.MessagesChatsBox) error {
		require.Len(t, req.ID, 1)
		input, ok := req.ID[0].(*tg.InputChannel)
		require.True(t, ok)
		assert.Equal(t, id, input.ChannelID)
		out.Chats = &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Channel{ID: id, AccessHash: accessHash}}}
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

func TestEditMessageIDRangeThroughMCP(t *testing.T) {
	inv := telegramfake.New(
		notUserStep(t, 41),
		resolveChannelStep(t, 41, 91),
		telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesEditMessageRequest, out *tg.UpdatesBox) error {
			// Check the target after TL serialisation, where a Go int could
			// otherwise silently wrap to another message's signed int32 ID.
			var buf bin.Buffer
			require.NoError(t, rpc.Encode(&buf))
			var decoded tg.MessagesEditMessageRequest
			require.NoError(t, decoded.Decode(&buf))
			require.Equal(t, math.MaxInt32, decoded.ID)
			out.Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateEditChannelMessage{Message: &tg.Message{ID: decoded.ID, Date: 11}}}}
			return nil
		}),
	)
	cs := connectToolClient(t, func(s *mcp.Server) {
		NewMessageEditHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).Register(s)
	})
	for _, id := range []string{"2147483648", "4294967338", "s:2147483648", "s:4294967338"} {
		res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "EditMessage", Arguments: map[string]any{
			"chat_id": 41, "message_id": id, "new_text": "updated",
		}})
		require.NoError(t, err)
		require.True(t, res.IsError)
		assert.Contains(t, toolResultText(res), "message_id")
		require.Empty(t, inv.RequestTypes(), "invalid target reached Telegram")
	}
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "EditMessage", Arguments: map[string]any{
		"chat_id": 41, "message_id": "2147483647", "new_text": "updated",
	}})
	require.NoError(t, err)
	require.False(t, res.IsError, "%+v", res.Content)
	assert.Zero(t, inv.Remaining())
}

func TestDestructiveHandlersFailClosedBeforeTelegramRPC(t *testing.T) {
	ctx := t.Context()
	req := &mcp.CallToolRequest{}

	tests := []struct {
		name string
		call func(*tg.Client) *mcp.CallToolResult
	}{
		{
			name: "delete messages",
			call: func(client *tg.Client) *mcp.CallToolResult {
				got, _, err := NewMessageDeleteHandler(tgclient.NewResolver(t.Context(), client)).handle(ctx, req, DeleteMessagesInput{ChatID: 1, MessageIDs: []string{"42"}})
				require.NoError(t, err)
				return got
			},
		},
		{
			name: "forward message",
			call: func(client *tg.Client) *mcp.CallToolResult {
				got, _, err := NewMessageForwardHandler(tgclient.NewResolver(t.Context(), client)).handle(ctx, req, ForwardMessageInput{FromChatID: 1, ToChatID: 2, MessageID: "42"})
				require.NoError(t, err)
				return got
			},
		},
		{
			name: "delete folder",
			call: func(client *tg.Client) *mcp.CallToolResult {
				got, _, err := NewDeleteFolderHandler(client).handle(ctx, req, DeleteFolderInput{FolderID: 2})
				require.NoError(t, err)
				return got
			},
		},
		{
			name: "leave chat",
			call: func(client *tg.Client) *mcp.CallToolResult {
				got, _, err := NewLeaveChatHandler(tgclient.NewResolver(t.Context(), client)).handle(ctx, req, LeaveChatInput{Chat: "@channel"})
				require.NoError(t, err)
				return got
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := telegramfake.New()
			got := tt.call(tg.NewClient(inv))
			require.NotNil(t, got)
			assert.True(t, got.IsError)
			assert.Contains(t, toolResultText(got), "confirm=true")
			assert.Empty(t, inv.RequestTypes(), "unconfirmed mutation reached Telegram")
		})
	}
}

func TestMessageMutationHandlersUseExpectedRPCs(t *testing.T) {
	const channelID = int64(41)
	const accessHash = int64(91)
	chatID := channelID
	req := &mcp.CallToolRequest{}

	t.Run("send", func(t *testing.T) {
		inv := telegramfake.New(
			notUserStep(t, channelID),
			resolveChannelStep(t, channelID, accessHash),
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesSendMessageRequest, out *tg.UpdatesBox) error {
				assert.Equal(t, "hello", rpc.Message)
				out.Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 7, Date: 10}}}}
				return nil
			}),
		)
		errRes, out, err := NewMessageSendHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).handle(t.Context(), req, SendMessageInput{ChatID: chatID, Message: "hello"})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, "7", out.MessageID)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("edit", func(t *testing.T) {
		inv := telegramfake.New(
			notUserStep(t, channelID),
			resolveChannelStep(t, channelID, accessHash),
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesEditMessageRequest, out *tg.UpdatesBox) error {
				assert.Equal(t, 7, rpc.ID)
				assert.Equal(t, "updated", rpc.Message)
				out.Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateEditChannelMessage{Message: &tg.Message{ID: 7, Date: 11}}}}
				return nil
			}),
		)
		errRes, out, err := NewMessageEditHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).handle(t.Context(), req, EditMessageInput{ChatID: chatID, MessageID: "7", NewText: "updated"})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, "7", out.MessageID)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("reaction", func(t *testing.T) {
		inv := telegramfake.New(
			notUserStep(t, channelID),
			resolveChannelStep(t, channelID, accessHash),
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesSendReactionRequest, out *tg.UpdatesBox) error {
				assert.Equal(t, 7, rpc.MsgID)
				reactions, ok := rpc.GetReaction()
				require.True(t, ok)
				require.Len(t, reactions, 1)
				out.Updates = &tg.Updates{}
				return nil
			}),
		)
		errRes, out, err := NewSetReactionHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).handle(t.Context(), req, SetReactionInput{ChatID: chatID, MessageID: "7", Emojis: []string{"👍"}})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, "7", out.MessageID)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("delete", func(t *testing.T) {
		inv := telegramfake.New(
			notUserStep(t, channelID),
			resolveChannelStep(t, channelID, accessHash),
			telegramfake.Typed(func(_ context.Context, rpc *tg.ChannelsGetMessagesRequest, out *tg.MessagesMessagesBox) error {
				assert.Len(t, rpc.ID, 1)
				msg := &tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: channelID}}
				msg.SetOut(true)
				out.Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{msg}, Chats: []tg.ChatClass{&tg.Channel{ID: channelID, AccessHash: accessHash}}}
				return nil
			}),
			telegramfake.Typed(func(_ context.Context, rpc *tg.ChannelsDeleteMessagesRequest, out *tg.MessagesAffectedMessages) error {
				assert.Equal(t, []int{7}, rpc.ID)
				out.Pts = 12
				return nil
			}),
			telegramfake.Typed(func(_ context.Context, rpc *tg.ChannelsGetMessagesRequest, out *tg.MessagesMessagesBox) error {
				assert.Len(t, rpc.ID, 1)
				out.Messages = &tg.MessagesChannelMessages{Messages: []tg.MessageClass{&tg.MessageEmpty{ID: 7}}}
				return nil
			}),
		)
		errRes, out, err := NewMessageDeleteHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).handle(t.Context(), req, DeleteMessagesInput{ChatID: chatID, MessageIDs: []string{"7"}, Confirm: true})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, 1, out.Deleted)
		assert.Equal(t, []DeletedMessage{{MessageID: "7", Status: statusDeleted}}, out.Results)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("delete scheduled", func(t *testing.T) {
		inv := telegramfake.New(
			notUserStep(t, channelID),
			resolveChannelStep(t, channelID, accessHash),
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesGetScheduledMessagesRequest, out *tg.MessagesMessagesBox) error {
				assert.Equal(t, []int{7}, rpc.ID)
				out.Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 7, PeerID: &tg.PeerChannel{ChannelID: channelID}}}}
				return nil
			}),
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesDeleteScheduledMessagesRequest, out *tg.UpdatesBox) error {
				assert.Equal(t, []int{7}, rpc.ID)
				out.Updates = &tg.Updates{}
				return nil
			}),
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesGetScheduledMessagesRequest, out *tg.MessagesMessagesBox) error {
				assert.Equal(t, []int{7}, rpc.ID)
				out.Messages = &tg.MessagesMessages{}
				return nil
			}),
		)
		errRes, out, err := NewMessageDeleteHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).handle(t.Context(), req, DeleteMessagesInput{ChatID: chatID, MessageIDs: []string{"s:7"}, Confirm: true})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, 1, out.Deleted)
		assert.Equal(t, []DeletedMessage{{MessageID: "s:7", Status: statusDeleted}}, out.Results)
		assert.Zero(t, inv.Remaining())
	})
}

func TestForwardAndMembershipMutationsUseExpectedRPCs(t *testing.T) {
	const sourceID = int64(41)
	const targetID = int64(42)

	t.Run("forward", func(t *testing.T) {
		inv := telegramfake.New(
			notUserStep(t, sourceID),
			resolveChannelStep(t, sourceID, 91),
			notUserStep(t, targetID),
			resolveChannelStep(t, targetID, 92),
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesForwardMessagesRequest, out *tg.UpdatesBox) error {
				assert.Equal(t, []int{7}, rpc.ID)
				out.Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 8, Date: 12}}}}
				return nil
			}),
		)
		errRes, out, err := NewMessageForwardHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).handle(t.Context(), &mcp.CallToolRequest{}, ForwardMessageInput{
			FromChatID: sourceID, MessageID: "7", ToChatID: targetID, Confirm: true,
		})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, "8", out.NewMessageID)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("forward re-resolves both peers once on a stale hash", func(t *testing.T) {
		var randomIDs []int64
		inv := telegramfake.New(
			notUserStep(t, sourceID),
			resolveChannelStep(t, sourceID, 91),
			notUserStep(t, targetID),
			resolveChannelStep(t, targetID, 92),
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesForwardMessagesRequest, _ *tg.UpdatesBox) error {
				randomIDs = append(randomIDs, rpc.RandomID...)
				return tgerr.New(400, "CHANNEL_INVALID")
			}),
			notUserStep(t, sourceID),
			resolveChannelStep(t, sourceID, 191),
			notUserStep(t, targetID),
			resolveChannelStep(t, targetID, 192),
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesForwardMessagesRequest, out *tg.UpdatesBox) error {
				assert.Equal(t, int64(191), rpc.FromPeer.(*tg.InputPeerChannel).AccessHash)
				assert.Equal(t, int64(192), rpc.ToPeer.(*tg.InputPeerChannel).AccessHash)
				randomIDs = append(randomIDs, rpc.RandomID...)
				out.Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 8, Date: 12}}}}
				return nil
			}),
		)
		errRes, out, err := NewMessageForwardHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).handle(t.Context(), &mcp.CallToolRequest{}, ForwardMessageInput{
			FromChatID: sourceID, MessageID: "7", ToChatID: targetID, Confirm: true,
		})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, "8", out.NewMessageID)
		require.Len(t, randomIDs, 2)
		assert.Equal(t, randomIDs[0], randomIDs[1], "the retry reuses the random id so Telegram deduplicates it")
		assert.Zero(t, inv.Remaining(), "exactly one retry")
	})

	t.Run("leave", func(t *testing.T) {
		inv := telegramfake.New(
			notUserStep(t, sourceID),
			resolveChannelStep(t, sourceID, 91),
			telegramfake.Typed(func(_ context.Context, rpc *tg.ChannelsLeaveChannelRequest, out *tg.UpdatesBox) error {
				input, ok := rpc.Channel.(*tg.InputChannel)
				require.True(t, ok)
				assert.Equal(t, sourceID, input.ChannelID)
				out.Updates = &tg.Updates{}
				return nil
			}),
		)
		errRes, out, err := NewLeaveChatHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).handle(t.Context(), &mcp.CallToolRequest{}, LeaveChatInput{
			Chat: strconv.FormatInt(sourceID, 10), Confirm: true,
		})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, statusLeft, out.Status)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("join by invite", func(t *testing.T) {
		inv := telegramfake.New(
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesImportChatInviteRequest, out *tg.MessagesChatInviteJoinResultBox) error {
				assert.Equal(t, "invite", rpc.Hash)
				out.ChatInviteJoinResult = &tg.MessagesChatInviteJoinResultOk{Updates: &tg.Updates{}}
				return nil
			}),
		)
		errRes, out, err := NewJoinChatHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).handle(t.Context(), &mcp.CallToolRequest{}, JoinChatInput{Chat: "+invite"})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, statusJoined, out.Status)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("join by username and by ID take one path", func(t *testing.T) {
		const channelID, hash = int64(71), int64(171)
		join := telegramfake.Typed(func(_ context.Context, rpc *tg.ChannelsJoinChannelRequest, out *tg.MessagesChatInviteJoinResultBox) error {
			assert.Equal(t, &tg.InputChannel{ChannelID: channelID, AccessHash: hash}, rpc.Channel)
			out.ChatInviteJoinResult = &tg.MessagesChatInviteJoinResultOk{Updates: &tg.Updates{}}
			return nil
		})
		inv := telegramfake.New(
			telegramfake.Typed(func(_ context.Context, _ *tg.ContactsResolveUsernameRequest, out *tg.ContactsResolvedPeer) error {
				out.Peer = &tg.PeerChannel{ChannelID: channelID}
				out.Chats = []tg.ChatClass{&tg.Channel{ID: channelID, AccessHash: hash, Title: "News", Broadcast: true}}
				return nil
			}),
			join,
			// The username resolution fed the cache: joining by ID needs no probe.
			join,
		)
		h := NewJoinChatHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv)))
		for _, chat := range []string{"@news", strconv.FormatInt(channelID, 10)} {
			errRes, out, err := h.handle(t.Context(), &mcp.CallToolRequest{}, JoinChatInput{Chat: chat})
			require.NoError(t, err)
			require.Nil(t, errRes)
			assert.Equal(t, statusJoined, out.Status)
			assert.Equal(t, channelID, out.ChatID)
			assert.Equal(t, "News", out.Title)
		}
		assert.Zero(t, inv.Remaining())
	})

	t.Run("join refuses a basic group by ID", func(t *testing.T) {
		const groupID = int64(72)
		inv := telegramfake.New(basicGroupSteps(t, groupID)...)
		errRes, out, err := NewJoinChatHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).handle(t.Context(), &mcp.CallToolRequest{}, JoinChatInput{Chat: strconv.FormatInt(groupID, 10)})
		require.NoError(t, err)
		require.Nil(t, out)
		require.NotNil(t, errRes)
		assert.Contains(t, toolResultText(errRes), "not a channel or supergroup")
		assert.Zero(t, inv.Remaining())
	})
}

func dialogFiltersStep(t *testing.T, filters ...tg.DialogFilterClass) telegramfake.InvokeFunc {
	t.Helper()
	return telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetDialogFiltersRequest, out *tg.MessagesDialogFilters) error {
		out.Filters = filters
		return nil
	})
}

func updateDialogFilterStep(t *testing.T, id int, expectFilter bool) telegramfake.InvokeFunc {
	t.Helper()
	return telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesUpdateDialogFilterRequest, out *tg.BoolBox) error {
		assert.Equal(t, id, rpc.ID)
		_, present := rpc.GetFilter()
		assert.Equal(t, expectFilter, present)
		out.Bool = &tg.BoolTrue{}
		return nil
	})
}

func TestFolderMutationHandlersUseExpectedRPCs(t *testing.T) {
	req := &mcp.CallToolRequest{}

	t.Run("create", func(t *testing.T) {
		inv := telegramfake.New(
			dialogFiltersStep(t, &tg.DialogFilter{ID: 2, Title: tg.TextWithEntities{Text: "Old"}}),
			updateDialogFilterStep(t, 3, true),
		)
		errRes, out, err := NewCreateFolderHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).handle(t.Context(), req, CreateFolderInput{Title: "Work", IncludeGroups: true})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, 3, out.FolderID)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("delete", func(t *testing.T) {
		inv := telegramfake.New(
			dialogFiltersStep(t, &tg.DialogFilter{ID: 3, Title: tg.TextWithEntities{Text: "Work"}}),
			updateDialogFilterStep(t, 3, false),
		)
		errRes, out, err := NewDeleteFolderHandler(tg.NewClient(inv)).handle(t.Context(), req, DeleteFolderInput{FolderID: 3, Confirm: true})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, folderStatusDeleted, out.Status)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("add chat", func(t *testing.T) {
		const channelID = int64(51)
		inv := telegramfake.New(
			dialogFiltersStep(t, &tg.DialogFilter{ID: 3, Title: tg.TextWithEntities{Text: "Work"}, Groups: true}),
			notUserStep(t, channelID),
			resolveChannelStep(t, channelID, 151),
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesUpdateDialogFilterRequest, out *tg.BoolBox) error {
				filter, ok := rpc.GetFilter()
				require.True(t, ok)
				standard := filter.(*tg.DialogFilter)
				require.Len(t, standard.IncludePeers, 1)
				assert.Equal(t, channelID, standard.IncludePeers[0].(*tg.InputPeerChannel).ChannelID)
				out.Bool = &tg.BoolTrue{}
				return nil
			}),
		)
		errRes, out, err := NewAddChatsToFolderHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).handle(t.Context(), req, AddChatsToFolderInput{
			FolderID: 3, Chats: []string{strconv.FormatInt(channelID, 10)},
		})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, []int64{channelID}, out.Added)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("remove chat", func(t *testing.T) {
		const channelID = int64(52)
		inv := telegramfake.New(
			dialogFiltersStep(t, &tg.DialogFilter{
				ID: 3, Title: tg.TextWithEntities{Text: "Work"},
				IncludePeers: []tg.InputPeerClass{&tg.InputPeerChannel{ChannelID: channelID, AccessHash: 152}, &tg.InputPeerUser{UserID: 8}},
			}),
			notUserStep(t, channelID),
			resolveChannelStep(t, channelID, 152),
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesUpdateDialogFilterRequest, out *tg.BoolBox) error {
				filter, ok := rpc.GetFilter()
				require.True(t, ok)
				assert.Equal(t, []int64{8}, peerBareIDs(filter.(*tg.DialogFilter).IncludePeers))
				out.Bool = &tg.BoolTrue{}
				return nil
			}),
		)
		errRes, out, err := NewRemoveChatsFromFolderHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).handle(t.Context(), req, RemoveChatsFromFolderInput{
			FolderID: 3, Chats: []string{strconv.FormatInt(channelID, 10)},
		})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, []int64{channelID}, out.Removed)
		assert.Zero(t, inv.Remaining())
	})
}

func TestSetChatMuteUsesExpectedRPC(t *testing.T) {
	const channelID = int64(44)
	inv := telegramfake.New(
		notUserStep(t, channelID),
		resolveChannelStep(t, channelID, 94),
		telegramfake.Typed(func(_ context.Context, rpc *tg.AccountUpdateNotifySettingsRequest, out *tg.BoolBox) error {
			peer, ok := rpc.Peer.(*tg.InputNotifyPeer)
			require.True(t, ok)
			channel, ok := peer.Peer.(*tg.InputPeerChannel)
			require.True(t, ok)
			assert.Equal(t, channelID, channel.ChannelID)
			assert.Equal(t, muteForeverUntil, rpc.Settings.MuteUntil)
			out.Bool = &tg.BoolTrue{}
			return nil
		}),
	)
	errRes, out, err := NewChatMuteHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).handle(t.Context(), &mcp.CallToolRequest{}, SetChatMuteInput{
		ChatID: channelID, Muted: true,
	})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.NotNil(t, out)
	assert.True(t, out.Forever)
	assert.Zero(t, inv.Remaining())
}
