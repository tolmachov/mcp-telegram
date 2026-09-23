package tools

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	telegramfake "github.com/tolmachov/mcp-telegram/internal/testutil/telegram"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// channelHash returns the access hash of a channel InputPeer.
func channelHash(t *testing.T, peer tg.InputPeerClass) int64 {
	t.Helper()
	channel, ok := peer.(*tg.InputPeerChannel)
	require.True(t, ok, "peer %T is not a channel", peer)
	return channel.AccessHash
}

// TestLeaveChatRetriesStaleHash verifies a numeric chat reference whose
// resolved access hash Telegram rejects is re-resolved and left with the
// fresh one, instead of failing until the cached peer expires.
func TestLeaveChatRetriesStaleHash(t *testing.T) {
	leave := func(hash int64, err error) telegramfake.InvokeFunc {
		return telegramfake.Typed(func(_ context.Context, req *tg.ChannelsLeaveChannelRequest, out *tg.UpdatesBox) error {
			assert.Equal(t, hash, req.Channel.(*tg.InputChannel).AccessHash)
			if err != nil {
				return err
			}
			out.Updates = &tg.Updates{}
			return nil
		})
	}
	inv := telegramfake.New(
		notUserStep(t, 41),
		resolveChannelStep(t, 41, 1),
		leave(1, tgerr.New(400, "CHANNEL_INVALID")),
		notUserStep(t, 41),
		resolveChannelStep(t, 41, 2),
		leave(2, nil),
	)
	h := NewLeaveChatHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv)))
	errRes, out, err := h.handle(t.Context(), &mcp.CallToolRequest{}, LeaveChatInput{Chat: "41", Confirm: true})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.NotNil(t, out)
	assert.Equal(t, statusLeft, out.Status)
	assert.Zero(t, inv.Remaining())
}

// TestAddChatsToFolderRetriesStaleHash verifies a folder edit whose resolved
// access hash Telegram rejects re-resolves the chat and writes the folder
// from Telegram's copy again, so the chat is added once with the fresh hash.
func TestAddChatsToFolderRetriesStaleHash(t *testing.T) {
	existing := &tg.InputPeerUser{UserID: 7, AccessHash: 70}
	update := func(hash int64, err error) telegramfake.InvokeFunc {
		return telegramfake.Typed(func(_ context.Context, req *tg.MessagesUpdateDialogFilterRequest, out *tg.BoolBox) error {
			filter, ok := req.Filter.(*tg.DialogFilter)
			require.True(t, ok)
			require.Len(t, filter.IncludePeers, 2, "each attempt starts from the folder as Telegram holds it")
			assert.Equal(t, existing, filter.IncludePeers[0])
			assert.Equal(t, hash, channelHash(t, filter.IncludePeers[1]))
			if err != nil {
				return err
			}
			out.Bool = &tg.BoolTrue{}
			return nil
		})
	}
	inv := telegramfake.New(
		telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetDialogFiltersRequest, out *tg.MessagesDialogFilters) error {
			out.Filters = []tg.DialogFilterClass{&tg.DialogFilter{
				ID:           5,
				Title:        tg.TextWithEntities{Text: "Work"},
				IncludePeers: []tg.InputPeerClass{existing},
			}}
			return nil
		}),
		notUserStep(t, 41),
		resolveChannelStep(t, 41, 1),
		update(1, tgerr.New(400, "CHANNEL_INVALID")),
		notUserStep(t, 41),
		resolveChannelStep(t, 41, 2),
		update(2, nil),
	)
	h := NewAddChatsToFolderHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv)))
	errRes, out, err := h.handle(t.Context(), &mcp.CallToolRequest{}, AddChatsToFolderInput{FolderID: 5, Chats: []string{"41"}})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.NotNil(t, out)
	assert.Equal(t, []int64{41}, out.Added)
	assert.Zero(t, inv.Remaining())
}
