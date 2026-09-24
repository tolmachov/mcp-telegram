package tools

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	telegramfake "github.com/tolmachov/mcp-telegram/internal/testutil/telegram"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

func TestLimitedAndProgressWriters(t *testing.T) {
	var dst bytes.Buffer
	limited := newLimitedWriter(&dst, 4)
	n, err := limited.Write([]byte("abcdef"))
	assert.Equal(t, 4, n)
	assert.ErrorIs(t, err, errMediaTooLarge)
	assert.Equal(t, "abcd", dst.String())

	reports := make([]int64, 0, 1)
	progress := newProgressWriter(&bytes.Buffer{}, time.Hour, func(written int64) {
		reports = append(reports, written)
	})
	n, err = progress.Write([]byte("abc"))
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	n, err = progress.Write([]byte("de"))
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, []int64{3}, reports)
}

func TestBackupProgressBounds(t *testing.T) {
	from := time.Unix(100, 0)
	to := time.Unix(200, 0)
	dated, stop := startBackupProgress(t.Context(), &mcp.CallToolRequest{}, from, to, 0)
	assert.Zero(t, dated.percent(), "no message dated yet")
	dated.update(1, 5, time.Unix(150, 0))
	assert.Equal(t, float64(50), dated.percent())
	dated.update(2, 9, time.Unix(50, 0))
	dated.update(3, 9, time.Time{})
	assert.Equal(t, float64(100), dated.percent(), "an earlier message before the window caps at 100")
	dated.send("done")
	stop()

	counted, stop := startBackupProgress(t.Context(), &mcp.CallToolRequest{}, time.Time{}, time.Time{}, 2)
	counted.update(1, 3, time.Unix(50, 0))
	assert.Equal(t, float64(100), counted.percent())
	stop()
}

func TestMessagePageCursorRoundTripAndRejection(t *testing.T) {
	raw := formatMessagePageCursor(messagePageCursor{Kind: cursorKindSearch, ChatID: 9, OffsetID: 8, Limit: 50, Query: "needle"})
	parsed, err := parseMessagePageCursor(raw, cursorKindSearch)
	require.NoError(t, err)
	assert.Equal(t, "needle", parsed.Query)
	_, err = parseMessagePageCursor(raw, cursorKindHistory)
	assert.ErrorContains(t, err, "belongs to")

	invalid := formatMessagePageCursor(messagePageCursor{Kind: cursorKindSearch, ChatID: 9, OffsetID: 0, Limit: 50})
	_, err = parseMessagePageCursor(invalid, cursorKindSearch)
	assert.ErrorContains(t, err, "invalid pagination state")
}

func TestReadOnlyHandlersUseExpectedRPCs(t *testing.T) {
	t.Run("get me", func(t *testing.T) {
		inv := telegramfake.New(
			telegramfake.Typed(func(_ context.Context, _ *tg.UsersGetFullUserRequest, out *tg.UsersUserFull) error {
				out.FullUser = tg.UserFull{About: "bio"}
				out.Users = []tg.UserClass{&tg.User{ID: 1, Self: true, FirstName: "A", Username: "me"}}
				return nil
			}),
		)
		errRes, out, err := NewMeGetHandler(tg.NewClient(inv)).handle(t.Context(), &mcp.CallToolRequest{}, GetMeInput{})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, int64(1), out.ID)
		assert.Equal(t, "bio", out.Bio)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("resolve username", func(t *testing.T) {
		inv := telegramfake.New(
			telegramfake.Typed(func(_ context.Context, req *tg.ContactsResolveUsernameRequest, out *tg.ContactsResolvedPeer) error {
				assert.Equal(t, "public", req.Username)
				out.Users = []tg.UserClass{&tg.User{ID: 1, AccessHash: 11, Username: "public", FirstName: "A", Self: true}}
				out.Chats = []tg.ChatClass{&tg.Channel{ID: 2, AccessHash: 22, Username: "public", Title: "Public", Megagroup: true}}
				return nil
			}),
		)
		peers := tgclient.NewResolver(t.Context(), tg.NewClient(inv))
		errRes, out, err := NewUsernameResolveHandler(peers).handle(t.Context(), &mcp.CallToolRequest{}, ResolveUsernameInput{Username: "@public"})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.Len(t, out.Entities, 2)
		assert.Equal(t, tgdata.ChatTypeSupergroup, out.Entities[1].Kind)
		// Both entities resolve from the cache: the script has no probe left.
		for _, entity := range out.Entities {
			_, err := peers.Resolve(t.Context(), entity.ID)
			require.NoError(t, err)
		}
		assert.Zero(t, inv.Remaining())
	})

	t.Run("folders", func(t *testing.T) {
		inv := telegramfake.New(dialogFiltersStep(t,
			&tg.DialogFilterDefault{},
			&tg.DialogFilter{ID: 2, Title: tg.TextWithEntities{Text: "Work"}, IncludePeers: []tg.InputPeerClass{&tg.InputPeerUser{UserID: 7}}},
			&tg.DialogFilterChatlist{ID: 3, Title: tg.TextWithEntities{Text: "Shared"}},
		))
		errRes, out, err := NewGetFoldersHandler(tg.NewClient(inv)).handle(t.Context(), &mcp.CallToolRequest{}, GetFoldersInput{})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.Len(t, out.Folders, 2)
		assert.Equal(t, []int64{7}, out.Folders[0].IncludeIDs)
		assert.Equal(t, folderKindShared, out.Folders[1].Kind)
		assert.Zero(t, inv.Remaining())
	})

	t.Run("chat info", func(t *testing.T) {
		const channelID = int64(60)
		inv := telegramfake.New(
			notUserStep(t, channelID),
			resolveChannelStep(t, channelID, 160),
			telegramfake.Typed(func(_ context.Context, req *tg.ChannelsGetFullChannelRequest, out *tg.MessagesChatFull) error {
				channel := req.Channel.(*tg.InputChannel)
				assert.Equal(t, channelID, channel.ChannelID)
				out.FullChat = &tg.ChannelFull{About: "description", ParticipantsCount: 10}
				out.Chats = []tg.ChatClass{&tg.Channel{ID: channelID, AccessHash: 160, Title: "Detailed", Username: "detailed", Megagroup: true}}
				return nil
			}),
			telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetPeerDialogsRequest, out *tg.MessagesPeerDialogs) error {
				out.Dialogs = []tg.DialogClass{&tg.Dialog{UnreadCount: 3, Pinned: true}}
				return nil
			}),
		)
		errRes, out, err := NewChatInfoGetHandler(tgclient.NewResolver(t.Context(), tg.NewClient(inv))).handle(t.Context(), &mcp.CallToolRequest{}, GetChatInfoInput{ChatID: channelID})
		require.NoError(t, err)
		require.Nil(t, errRes)
		require.NotNil(t, out)
		assert.Equal(t, "Detailed", out.Name)
		assert.Equal(t, 10, out.MembersCount)
		assert.Equal(t, 3, out.UnreadCount)
		assert.Zero(t, inv.Remaining())
	})
}

func TestTextResult(t *testing.T) {
	assert.Equal(t, "ok", textResult("ok").Content[0].(*mcp.TextContent).Text)
}

func TestChatsHandlersServeSeededSnapshot(t *testing.T) {
	cache, snap := seededChatsCache(t, []tgdata.ChatInfo{
		{ID: 1, Name: "Alpha", Username: "alpha"},
		{ID: 2, Name: "Beta", Username: "beta"},
		{ID: 3, Name: "Gamma", Username: "gamma"},
	}, true)

	get := NewChatsGetHandler(cache)
	first := get.pageFrom(snap, 0, 2)
	require.True(t, first.HasMore)
	assert.NotEmpty(t, first.NextCursor)
	errRes, second, err := get.handle(t.Context(), &mcp.CallToolRequest{}, GetChatsInput{Limit: 2, Cursor: first.NextCursor})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.Len(t, second.Chats, 1)
	assert.Equal(t, int64(3), second.Chats[0].ID)
	assert.Contains(t, second.Warning, "incomplete")

	errRes, _, err = get.handle(t.Context(), &mcp.CallToolRequest{}, GetChatsInput{Cursor: "invalid"})
	require.NoError(t, err)
	require.NotNil(t, errRes)

	peers, _ := globalSearchPeers(t, nil, nil)
	search := NewChatsSearchHandler(peers, cache)
	errRes, found, err := search.handle(t.Context(), &mcp.CallToolRequest{}, SearchChatsInput{Query: "@beta", Limit: 1})
	require.NoError(t, err)
	require.Nil(t, errRes)
	require.Len(t, found.Results, 1)
	assert.Equal(t, int64(2), found.Results[0].ID)
	assert.Contains(t, found.Warning, "incomplete")
}

func TestBackupMessagesWritesAtomicallyInsideConfiguredPath(t *testing.T) {
	const channelID = int64(61)
	dir := t.TempDir()
	target := filepath.Join(dir, "backup.txt")
	inv := telegramfake.New(
		notUserStep(t, channelID),
		resolveChannelStep(t, channelID, 161),
		notUserStep(t, channelID),
		resolveChannelStep(t, channelID, 161),
		telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetHistoryRequest, out *tg.MessagesMessagesBox) error {
			out.Messages = &tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 1, Date: 100, Message: "backed up"}}}
			return nil
		}),
	)
	client := tg.NewClient(inv)
	provider := messages.NewProvider(tgclient.NewResolver(t.Context(), client), 100_000)
	handler := NewMessageBackupHandler(tgclient.NewResolver(t.Context(), client), provider, []string{dir})

	errRes, out, err := handler.handle(t.Context(), &mcp.CallToolRequest{}, BackupMessagesInput{
		ChatID: channelID, Filepath: target, Limit: 1,
	})
	require.NoError(t, err)
	require.NotNil(t, errRes)
	assert.False(t, errRes.IsError)
	require.NotNil(t, out)
	assert.Equal(t, 1, out.MessageCount)
	content, err := os.ReadFile(target) //nolint:gosec // target is inside the test's private temporary directory.
	require.NoError(t, err)
	assert.Contains(t, string(content), "backed up")
	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.Zero(t, inv.Remaining())
}

// partialBackupScript resolves channelID, returns a first history page of two
// messages out of ten, and answers the next page with secondPage.
func partialBackupScript(t *testing.T, channelID int64, secondPage func() error) *telegramfake.Invoker {
	t.Helper()
	return telegramfake.New(
		notUserStep(t, channelID),
		resolveChannelStep(t, channelID, 161),
		telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetHistoryRequest, out *tg.MessagesMessagesBox) error {
			out.Messages = &tg.MessagesMessagesSlice{Count: 10, Messages: []tg.MessageClass{
				&tg.Message{ID: 10, Date: 200, Message: "second"},
				&tg.Message{ID: 9, Date: 100, Message: "first"},
			}}
			return nil
		}),
		telegramfake.Typed(func(context.Context, *tg.MessagesGetHistoryRequest, *tg.MessagesMessagesBox) error {
			return secondPage()
		}),
	)
}

// TestBackupMessagesSavesPartialOnFloodWait verifies a flood wait after the
// first page still saves what was fetched, and the failure names both the
// wait and the saved file.
func TestBackupMessagesSavesPartialOnFloodWait(t *testing.T) {
	const channelID = int64(62)
	dir := t.TempDir()
	target := filepath.Join(dir, "backup.txt")
	inv := partialBackupScript(t, channelID, func() error {
		return &tgerr.Error{Code: 420, Message: "FLOOD_WAIT_30", Type: "FLOOD_WAIT", Argument: 30}
	})
	peers := tgclient.NewResolver(t.Context(), tg.NewClient(inv))
	handler := NewMessageBackupHandler(peers, messages.NewProvider(peers, 100_000), []string{dir})

	errRes, out, err := handler.handle(t.Context(), &mcp.CallToolRequest{}, BackupMessagesInput{ChatID: channelID, Filepath: target})
	require.Error(t, err)
	assert.Nil(t, errRes)
	assert.Nil(t, out)
	text := failureText("BackupMessages", err)
	assert.Contains(t, text, "30 seconds")
	assert.Contains(t, text, "a partial file with 2 messages was saved to "+target)
	content, readErr := os.ReadFile(target) //nolint:gosec // target is inside the test's private temporary directory.
	require.NoError(t, readErr)
	assert.Contains(t, string(content), "first")
	assert.Contains(t, string(content), "second")
	assert.Zero(t, inv.Remaining())
}

// TestBackupMessagesSavesPartialOnCancel verifies a cancelled backup saves
// what was fetched and reports it as a partial success, not an error.
func TestBackupMessagesSavesPartialOnCancel(t *testing.T) {
	const channelID = int64(63)
	dir := t.TempDir()
	target := filepath.Join(dir, "backup.txt")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	inv := partialBackupScript(t, channelID, func() error {
		cancel()
		return context.Canceled
	})
	peers := tgclient.NewResolver(t.Context(), tg.NewClient(inv))
	handler := NewMessageBackupHandler(peers, messages.NewProvider(peers, 100_000), []string{dir})

	res, out, err := handler.handle(ctx, &mcp.CallToolRequest{}, BackupMessagesInput{ChatID: channelID, Filepath: target})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
	require.NotNil(t, out)
	assert.True(t, out.Partial)
	assert.Equal(t, 2, out.MessageCount)
	assert.Equal(t, target, out.Filepath)
	_, statErr := os.Stat(target)
	require.NoError(t, statErr)
	assert.Zero(t, inv.Remaining())
}

// TestBackupMessagesReportsAClientStopAsAFailure verifies a backup whose
// fetch the Telegram client's stop cancelled, while the call itself still
// runs, saves what was fetched and reports the stop as a failure, not as the
// caller's cancel.
func TestBackupMessagesReportsAClientStopAsAFailure(t *testing.T) {
	const channelID = int64(65)
	dir := t.TempDir()
	target := filepath.Join(dir, "backup.txt")
	inv := partialBackupScript(t, channelID, func() error { return context.Canceled })
	peers := tgclient.NewResolver(t.Context(), tg.NewClient(inv))
	handler := NewMessageBackupHandler(peers, messages.NewProvider(peers, 100_000), []string{dir})

	res, out, err := handler.handle(t.Context(), &mcp.CallToolRequest{}, BackupMessagesInput{ChatID: channelID, Filepath: target})
	require.Error(t, err)
	assert.Nil(t, res)
	assert.Nil(t, out)
	text := failureText("BackupMessages", err)
	assert.Contains(t, text, "The backup stopped mid-stream; a partial file with 2 messages was saved to "+target)
	assert.NotContains(t, text, "cancelled; partial file saved")
	_, statErr := os.Stat(target)
	require.NoError(t, statErr)
	assert.Zero(t, inv.Remaining())
}

// TestBackupMessagesSavesPartialOnTimeout verifies a backup cut short by a
// deadline tells the model where the partial file is: the outcome rides in
// the note, which a systemic failure keeps.
func TestBackupMessagesSavesPartialOnTimeout(t *testing.T) {
	const channelID = int64(64)
	dir := t.TempDir()
	target := filepath.Join(dir, "backup.txt")
	inv := partialBackupScript(t, channelID, func() error { return context.DeadlineExceeded })
	peers := tgclient.NewResolver(t.Context(), tg.NewClient(inv))
	handler := NewMessageBackupHandler(peers, messages.NewProvider(peers, 100_000), []string{dir})

	_, out, err := handler.handle(t.Context(), &mcp.CallToolRequest{}, BackupMessagesInput{ChatID: channelID, Filepath: target})
	require.Error(t, err)
	assert.Nil(t, out)
	text := failureText("BackupMessages", err)
	assert.Contains(t, text, "context deadline exceeded")
	assert.Contains(t, text, "a partial file with 2 messages was saved to "+target)
	assert.Zero(t, inv.Remaining())
}
