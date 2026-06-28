package tgdata

import (
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
