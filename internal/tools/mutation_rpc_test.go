package tools

import (
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	telegramfake "github.com/tolmachov/mcp-telegram/internal/testutil/telegram"
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

func botAPIChannelID(id int64) int64 {
	return -1_000_000_000_000 - id
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
				got, _, err := NewMessageDeleteHandler(client).handle(ctx, req, DeleteMessagesInput{ChatID: 1, MessageIDs: []string{"42"}})
				require.NoError(t, err)
				return got
			},
		},
		{
			name: "forward message",
			call: func(client *tg.Client) *mcp.CallToolResult {
				got, _, err := NewMessageForwardHandler(client).handle(ctx, req, ForwardMessageInput{FromChatID: 1, ToChatID: 2, MessageID: "42"})
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
				got, _, err := NewLeaveChatHandler(client).handle(ctx, req, LeaveChatInput{Chat: "@channel"})
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
	chatID := botAPIChannelID(channelID)
	req := &mcp.CallToolRequest{}

	t.Run("send", func(t *testing.T) {
		inv := telegramfake.New(
			resolveChannelStep(t, channelID, accessHash),
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesSendMessageRequest, out *tg.UpdatesBox) error {
				assert.Equal(t, "hello", rpc.Message)
				out.Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 7, Date: 10}}}}
				return nil
			}),
		)
		errRes, out, err := NewMessageSendHandler(tg.NewClient(inv)).handle(t.Context(), req, SendMessageInput{ChatID: chatID, Message: "hello"})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, "7", out.MessageID)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("edit", func(t *testing.T) {
		inv := telegramfake.New(
			resolveChannelStep(t, channelID, accessHash),
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesEditMessageRequest, out *tg.UpdatesBox) error {
				assert.Equal(t, 7, rpc.ID)
				assert.Equal(t, "updated", rpc.Message)
				out.Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateEditChannelMessage{Message: &tg.Message{ID: 7, Date: 11}}}}
				return nil
			}),
		)
		errRes, out, err := NewMessageEditHandler(tg.NewClient(inv)).handle(t.Context(), req, EditMessageInput{ChatID: chatID, MessageID: "7", NewText: "updated"})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, "7", out.MessageID)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("reaction", func(t *testing.T) {
		inv := telegramfake.New(
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
		errRes, out, err := NewSetReactionHandler(tg.NewClient(inv)).handle(t.Context(), req, SetReactionInput{ChatID: chatID, MessageID: "7", Emojis: []string{"👍"}})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, "7", out.MessageID)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("delete", func(t *testing.T) {
		inv := telegramfake.New(
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
		errRes, out, err := NewMessageDeleteHandler(tg.NewClient(inv)).handle(t.Context(), req, DeleteMessagesInput{ChatID: chatID, MessageIDs: []string{"7"}, Confirm: true})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, 1, out.Deleted)
		assert.Equal(t, []DeletedMessage{{MessageID: "7", Status: statusDeleted}}, out.Results)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("delete scheduled", func(t *testing.T) {
		inv := telegramfake.New(
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
		errRes, out, err := NewMessageDeleteHandler(tg.NewClient(inv)).handle(t.Context(), req, DeleteMessagesInput{ChatID: chatID, MessageIDs: []string{"s:7"}, Confirm: true})
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
			resolveChannelStep(t, sourceID, 91),
			resolveChannelStep(t, targetID, 92),
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesForwardMessagesRequest, out *tg.UpdatesBox) error {
				assert.Equal(t, []int{7}, rpc.ID)
				out.Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 8, Date: 12}}}}
				return nil
			}),
		)
		errRes, out, err := NewMessageForwardHandler(tg.NewClient(inv)).handle(t.Context(), &mcp.CallToolRequest{}, ForwardMessageInput{
			FromChatID: botAPIChannelID(sourceID), MessageID: "7", ToChatID: botAPIChannelID(targetID), Confirm: true,
		})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, "8", out.NewMessageID)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("leave", func(t *testing.T) {
		inv := telegramfake.New(
			resolveChannelStep(t, sourceID, 91),
			telegramfake.Typed(func(_ context.Context, rpc *tg.ChannelsLeaveChannelRequest, out *tg.UpdatesBox) error {
				input, ok := rpc.Channel.(*tg.InputChannel)
				require.True(t, ok)
				assert.Equal(t, sourceID, input.ChannelID)
				out.Updates = &tg.Updates{}
				return nil
			}),
		)
		errRes, out, err := NewLeaveChatHandler(tg.NewClient(inv)).handle(t.Context(), &mcp.CallToolRequest{}, LeaveChatInput{
			Chat: botAPIChannelIDString(sourceID), Confirm: true,
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
		errRes, out, err := NewJoinChatHandler(tg.NewClient(inv)).handle(t.Context(), &mcp.CallToolRequest{}, JoinChatInput{Chat: "+invite"})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, statusJoined, out.Status)
		assert.Zero(t, inv.Remaining())
	})
}

func botAPIChannelIDString(id int64) string {
	return fmt.Sprintf("%d", botAPIChannelID(id))
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
		errRes, out, err := NewCreateFolderHandler(tg.NewClient(inv)).handle(t.Context(), req, CreateFolderInput{Title: "Work", IncludeGroups: true})
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
		errRes, out, err := NewAddChatsToFolderHandler(tg.NewClient(inv)).handle(t.Context(), req, AddChatsToFolderInput{
			FolderID: 3, Chats: []string{botAPIChannelIDString(channelID)},
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
			resolveChannelStep(t, channelID, 152),
			telegramfake.Typed(func(_ context.Context, rpc *tg.MessagesUpdateDialogFilterRequest, out *tg.BoolBox) error {
				filter, ok := rpc.GetFilter()
				require.True(t, ok)
				assert.Equal(t, []int64{8}, peerBareIDs(filter.(*tg.DialogFilter).IncludePeers))
				out.Bool = &tg.BoolTrue{}
				return nil
			}),
		)
		errRes, out, err := NewRemoveChatsFromFolderHandler(tg.NewClient(inv)).handle(t.Context(), req, RemoveChatsFromFolderInput{
			FolderID: 3, Chats: []string{botAPIChannelIDString(channelID)},
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
	errRes, out, err := NewChatMuteHandler(tg.NewClient(inv)).handle(t.Context(), &mcp.CallToolRequest{}, SetChatMuteInput{
		ChatID: botAPIChannelID(channelID), Muted: true,
	})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.NotNil(t, out)
	assert.True(t, out.Forever)
	assert.Zero(t, inv.Remaining())
}
