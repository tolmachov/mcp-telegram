package tgclient

import (
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInvoker dispatches MTProto calls to per-request-type handlers so
// ResolvePeer can be exercised without a network connection. A nil handler
// behaves as "not found" for that peer type, so each test sets only the
// handlers it cares about.
type fakeInvoker struct {
	users    func(ids []tg.InputUserClass) ([]tg.UserClass, error)
	chats    func(ids []int64) (tg.MessagesChatsClass, error)
	channels func(ids []tg.InputChannelClass) (tg.MessagesChatsClass, error)
}

func (f fakeInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch req := input.(type) {
	case *tg.UsersGetUsersRequest:
		res := []tg.UserClass{&tg.UserEmpty{}}
		var err error
		if f.users != nil {
			res, err = f.users(req.ID)
		}
		if err != nil {
			return err
		}
		output.(*tg.UserClassVector).Elems = res
		return nil
	case *tg.MessagesGetChatsRequest:
		var res tg.MessagesChatsClass = &tg.MessagesChats{Chats: []tg.ChatClass{&tg.ChatEmpty{}}}
		var err error
		if f.chats != nil {
			res, err = f.chats(req.ID)
		}
		if err != nil {
			return err
		}
		output.(*tg.MessagesChatsBox).Chats = res
		return nil
	case *tg.ChannelsGetChannelsRequest:
		if f.channels == nil {
			return tgerr.New(400, "CHANNEL_INVALID")
		}
		res, err := f.channels(req.ID)
		if err != nil {
			return err
		}
		output.(*tg.MessagesChatsBox).Chats = res
		return nil
	default:
		return fmt.Errorf("fakeInvoker: unexpected request %T", input)
	}
}

func TestResolvePeer(t *testing.T) {
	ctx := context.Background()

	t.Run("bare positive channel id resolves as channel", func(t *testing.T) {
		var gotChannelID int64
		client := tg.NewClient(fakeInvoker{
			channels: func(ids []tg.InputChannelClass) (tg.MessagesChatsClass, error) {
				gotChannelID = ids[0].(*tg.InputChannel).ChannelID
				return &tg.MessagesChats{Chats: []tg.ChatClass{
					&tg.Channel{ID: 1555091578, AccessHash: 999},
				}}, nil
			},
			chats: func([]int64) (tg.MessagesChatsClass, error) {
				t.Fatal("a matching channel must resolve before the basic-chat probe")
				return nil, nil
			},
		})

		peer, err := ResolvePeer(ctx, client, 1555091578)
		require.NoError(t, err)
		assert.Equal(t, &tg.InputPeerChannel{ChannelID: 1555091578, AccessHash: 999}, peer)
		assert.Equal(t, int64(1555091578), gotChannelID, "bare id must reach channels.getChannels unchanged")
	})

	t.Run("legacy -100 marked id strips prefix and resolves as channel", func(t *testing.T) {
		var gotChannelID int64
		client := tg.NewClient(fakeInvoker{
			users: func([]tg.InputUserClass) ([]tg.UserClass, error) {
				t.Fatal("marked id must not probe users.getUsers")
				return nil, nil
			},
			channels: func(ids []tg.InputChannelClass) (tg.MessagesChatsClass, error) {
				gotChannelID = ids[0].(*tg.InputChannel).ChannelID
				return &tg.MessagesChats{Chats: []tg.ChatClass{
					&tg.Channel{ID: 1555091578, AccessHash: 999},
				}}, nil
			},
		})

		peer, err := ResolvePeer(ctx, client, -1001555091578)
		require.NoError(t, err)
		assert.Equal(t, &tg.InputPeerChannel{ChannelID: 1555091578, AccessHash: 999}, peer)
		assert.Equal(t, int64(1555091578), gotChannelID, "-100 prefix must be stripped")
	})

	t.Run("legacy small-negative id resolves as basic chat", func(t *testing.T) {
		var gotChatID int64
		client := tg.NewClient(fakeInvoker{
			users: func([]tg.InputUserClass) ([]tg.UserClass, error) {
				t.Fatal("basic-chat hint must not probe users.getUsers")
				return nil, nil
			},
			channels: func([]tg.InputChannelClass) (tg.MessagesChatsClass, error) {
				t.Fatal("basic-chat hint must not probe channels.getChannels")
				return nil, nil
			},
			chats: func(ids []int64) (tg.MessagesChatsClass, error) {
				gotChatID = ids[0]
				return &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Chat{ID: 500}}}, nil
			},
		})

		peer, err := ResolvePeer(ctx, client, -500)
		require.NoError(t, err)
		assert.Equal(t, &tg.InputPeerChat{ChatID: 500}, peer)
		assert.Equal(t, int64(500), gotChatID, "-500 must normalise to bare 500")
	})

	t.Run("user id resolves as user", func(t *testing.T) {
		client := tg.NewClient(fakeInvoker{
			users: func([]tg.InputUserClass) ([]tg.UserClass, error) {
				return []tg.UserClass{&tg.User{ID: 42, AccessHash: 111}}, nil
			},
			channels: func([]tg.InputChannelClass) (tg.MessagesChatsClass, error) {
				t.Fatal("user must win before the channel probe")
				return nil, nil
			},
		})

		peer, err := ResolvePeer(ctx, client, 42)
		require.NoError(t, err)
		assert.Equal(t, &tg.InputPeerUser{UserID: 42, AccessHash: 111}, peer)
	})

	t.Run("basic chat id resolves as chat after channel falls through", func(t *testing.T) {
		client := tg.NewClient(fakeInvoker{
			// users nil → not a user; channels nil → CHANNEL_INVALID (fall through)
			chats: func([]int64) (tg.MessagesChatsClass, error) {
				return &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Chat{ID: 500}}}, nil
			},
		})

		peer, err := ResolvePeer(ctx, client, 500)
		require.NoError(t, err)
		assert.Equal(t, &tg.InputPeerChat{ChatID: 500}, peer)
	})

	t.Run("unknown id returns a friendly error without MTProto codes", func(t *testing.T) {
		client := tg.NewClient(fakeInvoker{}) // all probes report not-found

		_, err := ResolvePeer(ctx, client, 12345)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "-100")
		assert.NotContains(t, err.Error(), "CHANNEL_INVALID")
		assert.Contains(t, err.Error(), "ResolveUsername")
	})

	t.Run("known user without access_hash fails loudly and aborts the sweep", func(t *testing.T) {
		client := tg.NewClient(fakeInvoker{
			users: func([]tg.InputUserClass) ([]tg.UserClass, error) {
				return []tg.UserClass{&tg.User{ID: 42, AccessHash: 0}}, nil
			},
			channels: func([]tg.InputChannelClass) (tg.MessagesChatsClass, error) {
				t.Fatal("a terminal user error must abort before later probes")
				return nil, nil
			},
		})

		_, err := ResolvePeer(ctx, client, 42)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "access_hash")
	})

	t.Run("genuine error in an early probe aborts without firing later probes", func(t *testing.T) {
		client := tg.NewClient(fakeInvoker{
			users: func([]tg.InputUserClass) ([]tg.UserClass, error) {
				return nil, tgerr.New(420, "FLOOD_WAIT_30")
			},
			channels: func([]tg.InputChannelClass) (tg.MessagesChatsClass, error) {
				t.Fatal("flood-wait must not be amplified by extra probes")
				return nil, nil
			},
			chats: func([]int64) (tg.MessagesChatsClass, error) {
				t.Fatal("flood-wait must not be amplified by extra probes")
				return nil, nil
			},
		})

		_, err := ResolvePeer(ctx, client, 1555091578)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "FLOOD_WAIT", "the real error must surface, not a misattributed one")
	})

	t.Run("inaccessible channel surfaces an actionable error", func(t *testing.T) {
		client := tg.NewClient(fakeInvoker{
			channels: func([]tg.InputChannelClass) (tg.MessagesChatsClass, error) {
				return nil, tgerr.New(400, "CHANNEL_PRIVATE")
			},
			chats: func([]int64) (tg.MessagesChatsClass, error) {
				t.Fatal("CHANNEL_PRIVATE is a definite channel signal; must not fall through")
				return nil, nil
			},
		})

		_, err := ResolvePeer(ctx, client, 1555091578)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ResolveUsername")
		assert.NotContains(t, err.Error(), "CHANNEL_PRIVATE", "raw MTProto code must not leak")
	})
}

func TestNormalizeDialogID(t *testing.T) {
	tests := []struct {
		in       int64
		wantID   int64
		wantHint peerHint
	}{
		{1555091578, 1555091578, hintNone},
		{42, 42, hintNone},
		{-1001555091578, 1555091578, hintChannel},
		{-123456789, 123456789, hintBasicChat},
	}
	for _, tt := range tests {
		id, hint := normalizeDialogID(tt.in)
		assert.Equal(t, tt.wantID, id, "id for %d", tt.in)
		assert.Equal(t, tt.wantHint, hint, "hint for %d", tt.in)
	}
}
