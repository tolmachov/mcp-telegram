package tgdata

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

type dialogsInvoker struct{ err error }

func (d dialogsInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	if _, ok := input.(*tg.MessagesGetDialogsRequest); !ok {
		return fmt.Errorf("unexpected request %T", input)
	}
	if d.err != nil {
		return d.err
	}
	output.(*tg.MessagesDialogsBox).Dialogs = &tg.MessagesDialogs{
		Dialogs: []tg.DialogClass{
			&tg.Dialog{Peer: &tg.PeerUser{UserID: 1}, UnreadCount: 2, UnreadMentionsCount: 1, Pinned: true},
			&tg.Dialog{Peer: &tg.PeerChat{ChatID: 2}},
			&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 3}, FolderID: 1},
		},
		Users: []tg.UserClass{&tg.User{ID: 1, AccessHash: 11, FirstName: "Alice", Username: "alice", Bot: true}},
		Chats: []tg.ChatClass{
			&tg.Chat{ID: 2, Title: "Group"},
			&tg.Channel{ID: 3, AccessHash: 33, Title: "Channel", Username: "channel"},
		},
	}
	return nil
}

// TestOffsetPeerPhantomChat is the regression guard for the bug this file
// fixes: a legacy group whose entity is absent from the response must still
// yield a usable offset peer (InputPeerChat needs only the ID), instead of
// aborting pagination the way gotd's helper did.
func TestOffsetPeerPhantomChat(t *testing.T) {
	em := newEntityMaps(nil, nil) // empty maps == phantom dialog

	got := offsetPeer(&tg.PeerChat{ChatID: 4767644535}, em)

	require.IsType(t, &tg.InputPeerChat{}, got)
	assert.Equal(t, int64(4767644535), got.(*tg.InputPeerChat).ChatID)
}

func TestOffsetPeerUserAndChannel(t *testing.T) {
	em := newEntityMaps(
		[]tg.UserClass{&tg.User{ID: 10, AccessHash: 111}},
		[]tg.ChatClass{&tg.Channel{ID: 20, AccessHash: 222}},
	)

	user := offsetPeer(&tg.PeerUser{UserID: 10}, em)
	require.IsType(t, &tg.InputPeerUser{}, user)
	assert.Equal(t, int64(111), user.(*tg.InputPeerUser).AccessHash)

	channel := offsetPeer(&tg.PeerChannel{ChannelID: 20}, em)
	require.IsType(t, &tg.InputPeerChannel{}, channel)
	assert.Equal(t, int64(222), channel.(*tg.InputPeerChannel).AccessHash)
}

// Users/channels need an access hash; without their entity we cannot build a
// valid input peer, so we fall back to empty rather than fail.
func TestOffsetPeerMissingAccessHashFallsBackToEmpty(t *testing.T) {
	em := newEntityMaps(nil, nil)

	assert.IsType(t, &tg.InputPeerEmpty{}, offsetPeer(&tg.PeerUser{UserID: 10}, em))
	assert.IsType(t, &tg.InputPeerEmpty{}, offsetPeer(&tg.PeerChannel{ChannelID: 20}, em))
}

func TestDialogToChatInfoSkipsPhantom(t *testing.T) {
	em := newEntityMaps(nil, nil)
	dialog := &tg.Dialog{Peer: &tg.PeerChat{ChatID: 4767644535}}

	_, ok := em.dialogToChatInfo(dialog, time.Now())
	assert.False(t, ok, "dialog with missing entity should be skipped")
}

func TestDialogToChatInfoBareChannelIDAndSupergroup(t *testing.T) {
	em := newEntityMaps(nil, []tg.ChatClass{
		&tg.Channel{ID: 555, Title: "Orbit Internal", Megagroup: true},
	})
	dialog := &tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 555}}

	info, ok := em.dialogToChatInfo(dialog, time.Now())
	require.True(t, ok)
	assert.Equal(t, ChatTypeSupergroup, info.Type)
	assert.Equal(t, "Orbit Internal", info.Name)
	assert.Equal(t, int64(555), info.ID)
}

func TestDialogToChatInfoAllPeerTypesAndFlags(t *testing.T) {
	now := time.Now()
	em := newEntityMaps(
		[]tg.UserClass{&tg.User{ID: 1, FirstName: "Alice", Bot: true}},
		[]tg.ChatClass{&tg.Chat{ID: 2}, &tg.Channel{ID: 3, Title: "News"}},
	)
	user, ok := em.dialogToChatInfo(&tg.Dialog{
		Peer: &tg.PeerUser{UserID: 1}, UnreadCount: 2, UnreadMentionsCount: 1,
		Pinned: true, FolderID: 1, NotifySettings: tg.PeerNotifySettings{MuteUntil: int(now.Add(time.Hour).Unix())},
	}, now)
	require.True(t, ok)
	assert.Equal(t, ChatTypeBot, user.Type)
	assert.True(t, user.Muted)
	assert.True(t, user.Pinned)
	assert.True(t, user.Archived)

	group, ok := em.dialogToChatInfo(&tg.Dialog{Peer: &tg.PeerChat{ChatID: 2}}, now)
	require.True(t, ok)
	assert.Equal(t, ChatTypeGroup, group.Type)
	assert.Equal(t, "Unknown", group.Name)

	channel, ok := em.dialogToChatInfo(&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 3}}, now)
	require.True(t, ok)
	assert.Equal(t, ChatTypeChannel, channel.Type)

	_, ok = em.dialogToChatInfo(&tg.Dialog{}, now)
	assert.False(t, ok)
	assert.Equal(t, peerLookupKey{}, peerKeyOf(nil))
	assert.Equal(t, peerLookupKey{'u', 1}, peerKeyOf(&tg.PeerUser{UserID: 1}))
	assert.Equal(t, peerLookupKey{'c', 2}, peerKeyOf(&tg.PeerChat{ChatID: 2}))
	assert.Equal(t, peerLookupKey{'h', 3}, peerKeyOf(&tg.PeerChannel{ChannelID: 3}))
	assert.IsType(t, &tg.InputPeerEmpty{}, offsetPeer(nil, em))
}

func TestGetChatsOnePageAndError(t *testing.T) {
	peers := tgclient.NewResolver(t.Context(), tg.NewClient(dialogsInvoker{}))
	var progressCalls int
	result, err := GetChats(t.Context(), peers, func(int, string) { progressCalls++ })
	require.NoError(t, err)
	require.Len(t, result.Chats, 3)
	assert.Equal(t, 3, result.Count)
	assert.False(t, result.Truncated)
	assert.Equal(t, 1, progressCalls)
	assert.Equal(t, ChatTypeBot, result.Chats[0].Type)
	assert.Equal(t, ChatTypeGroup, result.Chats[1].Type)
	assert.Equal(t, ChatTypeChannel, result.Chats[2].Type)
	// The listing's entities fed the resolver: its fake answers nothing but
	// messages.getDialogs, so a probe would fail.
	for _, chat := range result.Chats {
		_, err := peers.Resolve(t.Context(), chat.ID)
		require.NoError(t, err, "chat %d must resolve from the cache", chat.ID)
	}

	_, err = GetChats(t.Context(), tgclient.NewResolver(t.Context(), tg.NewClient(dialogsInvoker{err: fmt.Errorf("boom")})), nil)
	assert.ErrorContains(t, err, "listing chats")
}
