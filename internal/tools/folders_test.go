package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	telegramfake "github.com/tolmachov/mcp-telegram/internal/testutil/telegram"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// TestNextFolderID verifies the smallest-free-ID >= 2 assignment.
func TestNextFolderID(t *testing.T) {
	cases := []struct {
		name     string
		existing []int
		want     int
	}{
		{"empty", nil, 2},
		{"only defaults", []int{0, 1}, 2},
		{"sequential from 2", []int{0, 1, 2, 3}, 4},
		{"gap at 2", []int{0, 1, 3, 4}, 2},
		{"gap in middle", []int{2, 4, 5}, 3},
		{"unordered", []int{5, 2, 3}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, nextFolderID(tc.existing))
		})
	}
}

// TestPeerBareID checks bare-ID extraction for each supported peer form.
func TestPeerBareID(t *testing.T) {
	cases := []struct {
		name     string
		peer     tg.InputPeerClass
		wantKind string
		wantID   int64
		wantOK   bool
	}{
		{"user", &tg.InputPeerUser{UserID: 42, AccessHash: 9}, "user", 42, true},
		{"chat", &tg.InputPeerChat{ChatID: 7}, "chat", 7, true},
		{"channel", &tg.InputPeerChannel{ChannelID: 100, AccessHash: 5}, "channel", 100, true},
		{"self", &tg.InputPeerSelf{}, "self", 0, true},
		{"empty unsupported", &tg.InputPeerEmpty{}, "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, id, ok := peerBareID(tc.peer)
			assert.Equal(t, tc.wantKind, kind)
			assert.Equal(t, tc.wantID, id)
			assert.Equal(t, tc.wantOK, ok)
		})
	}
}

// TestSamePeer verifies identity comparison ignores access_hash but respects kind.
func TestSamePeer(t *testing.T) {
	assert.True(t, samePeer(
		&tg.InputPeerChannel{ChannelID: 1, AccessHash: 10},
		&tg.InputPeerChannel{ChannelID: 1, AccessHash: 999}, // different hash, same peer
	))
	assert.True(t, samePeer(&tg.InputPeerUser{UserID: 5}, &tg.InputPeerUser{UserID: 5}))
	assert.False(t, samePeer(&tg.InputPeerUser{UserID: 5}, &tg.InputPeerUser{UserID: 6}))
	// Same numeric ID but different kinds must not match.
	assert.False(t, samePeer(&tg.InputPeerUser{UserID: 1}, &tg.InputPeerChat{ChatID: 1}))
	assert.False(t, samePeer(&tg.InputPeerChannel{ChannelID: 1}, &tg.InputPeerChat{ChatID: 1}))
	// Unsupported peer forms never match.
	assert.False(t, samePeer(&tg.InputPeerEmpty{}, &tg.InputPeerEmpty{}))
}

// TestContainsAndRemovePeer covers the include/exclude list mutation helpers.
func TestContainsAndRemovePeer(t *testing.T) {
	list := []tg.InputPeerClass{
		&tg.InputPeerUser{UserID: 1, AccessHash: 11},
		&tg.InputPeerChannel{ChannelID: 2, AccessHash: 22},
	}
	assert.True(t, containsPeer(list, &tg.InputPeerUser{UserID: 1}))
	assert.False(t, containsPeer(list, &tg.InputPeerUser{UserID: 3}))

	pruned := removePeer(list, &tg.InputPeerChannel{ChannelID: 2})
	require.Len(t, pruned, 1)
	assert.False(t, containsPeer(pruned, &tg.InputPeerChannel{ChannelID: 2}))
	assert.True(t, containsPeer(pruned, &tg.InputPeerUser{UserID: 1}))
}

// TestFilterHasInclusion verifies the empty-folder guard.
func TestFilterHasInclusion(t *testing.T) {
	assert.False(t, filterHasInclusion(&tg.DialogFilter{}))
	assert.True(t, filterHasInclusion(&tg.DialogFilter{Groups: true}))
	assert.True(t, filterHasInclusion(&tg.DialogFilter{
		IncludePeers: []tg.InputPeerClass{&tg.InputPeerUser{UserID: 1}},
	}))
	assert.True(t, filterHasInclusion(&tg.DialogFilter{
		PinnedPeers: []tg.InputPeerClass{&tg.InputPeerUser{UserID: 1}},
	}))
}

// TestPeerBareIDs maps a mixed peer slice and skips forms without a bare ID.
func TestPeerBareIDs(t *testing.T) {
	got := peerBareIDs([]tg.InputPeerClass{
		&tg.InputPeerUser{UserID: 1},
		&tg.InputPeerChannel{ChannelID: 2},
		&tg.InputPeerEmpty{}, // no bare ID — skipped
		&tg.InputPeerChat{ChatID: 3},
	})
	assert.Equal(t, []int64{1, 2, 3}, got)
}

// TestFolderIDs collects standard and shared folder IDs and skips the default.
func TestFolderIDs(t *testing.T) {
	filters := &tg.MessagesDialogFilters{Filters: []tg.DialogFilterClass{
		&tg.DialogFilterDefault{}, // "All chats" — no ID, skipped
		&tg.DialogFilter{ID: 2},
		&tg.DialogFilterChatlist{ID: 5},
	}}
	assert.Equal(t, []int{2, 5}, folderIDs(filters))
}

// TestMapDialogFilter verifies the flag/kind mapping and the default-skip.
func TestMapDialogFilter(t *testing.T) {
	std, ok := mapDialogFilter(&tg.DialogFilter{
		ID:           2,
		Title:        tg.TextWithEntities{Text: "Work"},
		Broadcasts:   true, // maps to Flags.Channels
		Groups:       true,
		IncludePeers: []tg.InputPeerClass{&tg.InputPeerUser{UserID: 7}},
		ExcludePeers: []tg.InputPeerClass{&tg.InputPeerChat{ChatID: 8}},
	})
	require.True(t, ok)
	assert.Equal(t, folderKindStandard, std.Kind)
	assert.Equal(t, "Work", std.Title)
	require.NotNil(t, std.Flags)
	assert.True(t, std.Flags.Channels) // Broadcasts -> Channels alias
	assert.True(t, std.Flags.Groups)
	assert.Equal(t, []int64{7}, std.IncludeIDs)
	assert.Equal(t, []int64{8}, std.ExcludeIDs)

	shared, ok := mapDialogFilter(&tg.DialogFilterChatlist{ID: 5, Title: tg.TextWithEntities{Text: "Shared"}})
	require.True(t, ok)
	assert.Equal(t, folderKindShared, shared.Kind)
	assert.Nil(t, shared.Flags, "shared folders carry no category flags")

	_, ok = mapDialogFilter(&tg.DialogFilterDefault{})
	assert.False(t, ok, "the default All-chats view is omitted")
}

// TestApplyAdditions covers the add mutation: dedup against include AND pinned
// lists, and removal from the exclude list.
func TestApplyAdditions(t *testing.T) {
	filter := &tg.DialogFilter{
		IncludePeers: []tg.InputPeerClass{&tg.InputPeerUser{UserID: 1}},
		PinnedPeers:  []tg.InputPeerClass{&tg.InputPeerChannel{ChannelID: 2}},
		ExcludePeers: []tg.InputPeerClass{&tg.InputPeerChat{ChatID: 3}},
	}
	added, already := applyAdditions(filter, []tg.InputPeerClass{
		&tg.InputPeerUser{UserID: 1},       // already in include
		&tg.InputPeerChannel{ChannelID: 2}, // already pinned -> already present
		&tg.InputPeerChat{ChatID: 3},       // currently excluded -> added + un-excluded
		&tg.InputPeerUser{UserID: 9},       // brand new
	})
	assert.ElementsMatch(t, []int64{3, 9}, added)
	assert.ElementsMatch(t, []int64{1, 2}, already)
	assert.True(t, containsPeer(filter.IncludePeers, &tg.InputPeerChat{ChatID: 3}))
	assert.False(t, containsPeer(filter.ExcludePeers, &tg.InputPeerChat{ChatID: 3}), "added chat must leave the exclude list")
	assert.True(t, containsPeer(filter.IncludePeers, &tg.InputPeerUser{UserID: 9}))
}

// TestApplyRemovals covers the remove mutation across include and pinned lists.
func TestApplyRemovals(t *testing.T) {
	filter := &tg.DialogFilter{
		IncludePeers: []tg.InputPeerClass{&tg.InputPeerUser{UserID: 1}},
		PinnedPeers:  []tg.InputPeerClass{&tg.InputPeerChannel{ChannelID: 2}},
	}
	removed, notPresent := applyRemovals(filter, []tg.InputPeerClass{
		&tg.InputPeerUser{UserID: 1},       // in include
		&tg.InputPeerChannel{ChannelID: 2}, // pinned
		&tg.InputPeerUser{UserID: 9},       // not in folder
	})
	assert.ElementsMatch(t, []int64{1, 2}, removed)
	assert.Equal(t, []int64{9}, notPresent)
	assert.Empty(t, filter.IncludePeers)
	assert.Empty(t, filter.PinnedPeers)
}

// resolveFolderChat runs resolveFolderChats over the one reference ref and
// returns why it was skipped, or the failure that aborted the edit.
func resolveFolderChat(t *testing.T, peers *tgclient.Resolver, ref string) (reason string, fatal error) {
	t.Helper()
	var skipped []FolderSkippedChat
	resolved, fatal := resolveFolderChats(t.Context(), peers, []string{ref}, "update folder 2", &skipped)()
	if fatal != nil {
		return "", fatal
	}
	if len(skipped) == 0 {
		require.Len(t, resolved, 1)
		return "", nil
	}
	require.Len(t, skipped, 1)
	assert.Equal(t, ref, skipped[0].Chat)
	assert.Empty(t, resolved)
	return skipped[0].Reason, nil
}

// TestResolveFolderChatsLocalBranches covers the branches that return before
// any network call, so a resolver without a client is safe.
func TestResolveFolderChatsLocalBranches(t *testing.T) {
	peers := tgclient.NewResolver(t.Context(), nil)

	reason, fatal := resolveFolderChat(t, peers, "   ")
	assert.NoError(t, fatal)
	assert.Equal(t, "empty chat reference", reason)

	reason, fatal = resolveFolderChat(t, peers, "https://t.me/+AbCdEf")
	assert.NoError(t, fatal)
	assert.Contains(t, reason, "invite link")
}

// TestCreateFolderValidation covers input validation before any API call, so a
// nil *tg.Client is safe for every case here.
func TestCreateFolderValidation(t *testing.T) {
	h := &CreateFolderHandler{}
	ctx := context.Background()

	cases := []struct {
		name        string
		in          CreateFolderInput
		wantErrPart string
	}{
		{"empty title", CreateFolderInput{IncludeGroups: true}, "title is required"},
		{"blank title", CreateFolderInput{Title: "   ", IncludeGroups: true}, "title is required"},
		{"title too long", CreateFolderInput{Title: strings.Repeat("x", maxFolderTitleRunes+1), IncludeGroups: true}, "too long"},
		{"no inclusion source", CreateFolderInput{Title: "Work"}, "at least one chat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRes, _, err := h.handle(ctx, nil, tc.in)
			require.NoError(t, err)
			require.NotNil(t, errRes)
			require.True(t, errRes.IsError)
			assert.Contains(t, toolResultText(errRes), tc.wantErrPart)
		})
	}
}

// TestAddChatsToFolderValidation covers the nil-client validation layer.
func TestAddChatsToFolderValidation(t *testing.T) {
	h := &AddChatsToFolderHandler{}
	ctx := context.Background()

	cases := []struct {
		name        string
		in          AddChatsToFolderInput
		wantErrPart string
	}{
		{"zero folder id", AddChatsToFolderInput{Chats: []string{"@durov"}}, "folder_id is required"},
		{"negative folder id", AddChatsToFolderInput{FolderID: -1, Chats: []string{"@durov"}}, "folder_id is required"},
		{"empty chats", AddChatsToFolderInput{FolderID: 2}, "chats is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRes, _, err := h.handle(ctx, nil, tc.in)
			require.NoError(t, err)
			require.NotNil(t, errRes)
			require.True(t, errRes.IsError)
			assert.Contains(t, toolResultText(errRes), tc.wantErrPart)
		})
	}
}

// TestRemoveChatsFromFolderValidation covers the nil-client validation layer.
func TestRemoveChatsFromFolderValidation(t *testing.T) {
	h := &RemoveChatsFromFolderHandler{}
	ctx := context.Background()

	cases := []struct {
		name        string
		in          RemoveChatsFromFolderInput
		wantErrPart string
	}{
		{"zero folder id", RemoveChatsFromFolderInput{Chats: []string{"@durov"}}, "folder_id is required"},
		{"empty chats", RemoveChatsFromFolderInput{FolderID: 2}, "chats is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRes, _, err := h.handle(ctx, nil, tc.in)
			require.NoError(t, err)
			require.NotNil(t, errRes)
			require.True(t, errRes.IsError)
			assert.Contains(t, toolResultText(errRes), tc.wantErrPart)
		})
	}
}

// TestDeleteFolderValidation covers the nil-client validation layer.
func TestDeleteFolderValidation(t *testing.T) {
	h := &DeleteFolderHandler{}
	ctx := context.Background()

	errRes, _, err := h.handle(ctx, nil, DeleteFolderInput{FolderID: 0})
	require.NoError(t, err)
	require.NotNil(t, errRes)
	require.True(t, errRes.IsError)
	assert.Contains(t, toolResultText(errRes), "folder_id is required")
}

// TestResolveFolderChatsSkipsOnlyChatProblems pins that a folder edit skips a
// reference only for a problem with the reference itself, and fails on any
// other resolve failure so the model retries instead of losing a good chat.
func TestResolveFolderChatsSkipsOnlyChatProblems(t *testing.T) {
	resolveUsername := func(err error) telegramfake.InvokeFunc {
		return telegramfake.Typed(func(context.Context, *tg.ContactsResolveUsernameRequest, *tg.ContactsResolvedPeer) error { return err })
	}
	inv := telegramfake.New(
		resolveUsername(tgerr.New(400, "USERNAME_NOT_OCCUPIED")),
		resolveUsername(tgerr.New(500, "INTERNAL_SERVER_ERROR")),
	)
	peers := tgclient.NewResolver(t.Context(), tg.NewClient(inv))

	reason, fatal := resolveFolderChat(t, peers, "@nobody")
	require.NoError(t, fatal)
	assert.Contains(t, reason, "USERNAME_NOT_OCCUPIED")

	_, fatal = resolveFolderChat(t, peers, "@flaky")
	require.Error(t, fatal, "a server error says nothing about the chat")
	assert.Contains(t, failureText("AddChatsToFolder", fatal), "Failed to update folder 2")

	reason, fatal = resolveFolderChat(t, peers, "-1001555091578")
	require.NoError(t, fatal)
	assert.Contains(t, reason, "positive ID")

	reason, fatal = resolveFolderChat(t, peers, "@")
	require.NoError(t, fatal, "a lone @ is a bad reference, not a reason to abort the batch")
	assert.Contains(t, reason, "empty username")
	assert.Zero(t, inv.Remaining())
}

// TestResolveFolderChatsSkipsUnusableResolution pins that a @username Telegram
// resolves to nothing usable — a response without the entity its peer names,
// or a user without an access hash — is skipped as a bad reference.
func TestResolveFolderChatsSkipsUnusableResolution(t *testing.T) {
	resolveTo := func(resolved tg.ContactsResolvedPeer) telegramfake.InvokeFunc {
		return telegramfake.Typed(func(_ context.Context, _ *tg.ContactsResolveUsernameRequest, out *tg.ContactsResolvedPeer) error {
			*out = resolved
			return nil
		})
	}
	inv := telegramfake.New(
		resolveTo(tg.ContactsResolvedPeer{Peer: &tg.PeerUser{UserID: 7}}),
		resolveTo(tg.ContactsResolvedPeer{
			Peer:  &tg.PeerUser{UserID: 8},
			Users: []tg.UserClass{&tg.User{ID: 8, FirstName: "No Hash"}},
		}),
	)
	peers := tgclient.NewResolver(t.Context(), tg.NewClient(inv))

	reason, fatal := resolveFolderChat(t, peers, "@missing")
	require.NoError(t, fatal)
	assert.Contains(t, reason, "not present")

	reason, fatal = resolveFolderChat(t, peers, "@nohash")
	require.NoError(t, fatal)
	assert.Contains(t, reason, "access hash")
	assert.Zero(t, inv.Remaining())
}
