package tools

import (
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolvedInputPeer(t *testing.T) {
	t.Run("user with access hash", func(t *testing.T) {
		r := &tg.ContactsResolvedPeer{
			Peer:  &tg.PeerUser{UserID: 7},
			Users: []tg.UserClass{&tg.User{ID: 7, AccessHash: 99}},
		}
		peer, err := resolvedInputPeer(r)
		require.NoError(t, err)
		assert.Equal(t, &tg.InputPeerUser{UserID: 7, AccessHash: 99}, peer)
	})

	t.Run("user without access hash errors", func(t *testing.T) {
		r := &tg.ContactsResolvedPeer{
			Peer:  &tg.PeerUser{UserID: 7},
			Users: []tg.UserClass{&tg.User{ID: 7, AccessHash: 0}},
		}
		_, err := resolvedInputPeer(r)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "access hash")
	})

	t.Run("basic chat needs no access hash", func(t *testing.T) {
		r := &tg.ContactsResolvedPeer{Peer: &tg.PeerChat{ChatID: 42}}
		peer, err := resolvedInputPeer(r)
		require.NoError(t, err)
		assert.Equal(t, &tg.InputPeerChat{ChatID: 42}, peer)
	})

	t.Run("channel with access hash", func(t *testing.T) {
		r := &tg.ContactsResolvedPeer{
			Peer:  &tg.PeerChannel{ChannelID: 5},
			Chats: []tg.ChatClass{&tg.Channel{ID: 5, AccessHash: 123}},
		}
		peer, err := resolvedInputPeer(r)
		require.NoError(t, err)
		assert.Equal(t, &tg.InputPeerChannel{ChannelID: 5, AccessHash: 123}, peer)
	})

	t.Run("channel without access hash errors", func(t *testing.T) {
		r := &tg.ContactsResolvedPeer{
			Peer:  &tg.PeerChannel{ChannelID: 5},
			Chats: []tg.ChatClass{&tg.Channel{ID: 5, AccessHash: 0}},
		}
		_, err := resolvedInputPeer(r)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "access hash")
	})

	t.Run("channel missing from entities errors", func(t *testing.T) {
		r := &tg.ContactsResolvedPeer{Peer: &tg.PeerChannel{ChannelID: 5}}
		_, err := resolvedInputPeer(r)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not present")
	})
}

func TestResolvedChannel(t *testing.T) {
	t.Run("channel resolves to input channel and record", func(t *testing.T) {
		ch := &tg.Channel{ID: 5, AccessHash: 123, Title: "Chan"}
		r := &tg.ContactsResolvedPeer{
			Peer:  &tg.PeerChannel{ChannelID: 5},
			Chats: []tg.ChatClass{ch},
		}
		in, got, err := resolvedChannel(r)
		require.NoError(t, err)
		assert.Equal(t, &tg.InputChannel{ChannelID: 5, AccessHash: 123}, in)
		assert.Equal(t, ch, got)
	})

	t.Run("user is rejected as not a channel", func(t *testing.T) {
		r := &tg.ContactsResolvedPeer{
			Peer:  &tg.PeerUser{UserID: 7},
			Users: []tg.UserClass{&tg.User{ID: 7, AccessHash: 99}},
		}
		_, _, err := resolvedChannel(r)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a channel")
	})

	t.Run("basic chat is rejected as not a channel", func(t *testing.T) {
		r := &tg.ContactsResolvedPeer{Peer: &tg.PeerChat{ChatID: 42}}
		_, _, err := resolvedChannel(r)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a channel")
	})

	t.Run("channel without access hash errors", func(t *testing.T) {
		r := &tg.ContactsResolvedPeer{
			Peer:  &tg.PeerChannel{ChannelID: 5},
			Chats: []tg.ChatClass{&tg.Channel{ID: 5, AccessHash: 0}},
		}
		_, _, err := resolvedChannel(r)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "access hash")
	})

	t.Run("channel missing from entities errors", func(t *testing.T) {
		r := &tg.ContactsResolvedPeer{Peer: &tg.PeerChannel{ChannelID: 5}}
		_, _, err := resolvedChannel(r)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not present")
	})
}
