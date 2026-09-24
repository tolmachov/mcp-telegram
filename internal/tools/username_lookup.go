package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gotd/td/tg"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// This file is the single username-resolution seam for the tool handlers.
// Several tools (the folder edits, ResolveMessageLink, JoinChat/LeaveChat) turn
// a public @username into a peer, and each used to hand-roll the
// contacts.resolveUsername call and then guess which returned entity was the
// answer — one taking the first user, another the first chat. Because a single
// response can carry several entities (a bot inside a chat, a linked discussion
// group), those guesses could disagree and hand the same @username a different
// chat_id in different tools. The accessors below all key off r.Peer — the
// entity Telegram itself designates as the resolution — so every caller agrees.

// resolvePublicUsername normalises a public username (with or without a leading
// @), rejects empty input as a reference that names no chat
// (tgclient.ErrUnresolvablePeer), and calls contacts.resolveUsername. The
// entities of the answer feed peers' cache, so the IDs it hands out resolve
// without a probe.
func resolvePublicUsername(ctx context.Context, peers *tgclient.Resolver, username string) (*tg.ContactsResolvedPeer, error) {
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	if username == "" {
		return nil, fmt.Errorf("%w: empty username", tgclient.ErrUnresolvablePeer)
	}
	resolved, err := peers.Client().ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return nil, fmt.Errorf("resolving @%s: %w", username, err)
	}
	peers.Remember(resolved.Users, resolved.Chats)
	return resolved, nil
}

// resolvedEntity returns the response entity that r.Peer designates — a
// *tg.User, *tg.Chat or *tg.Channel — or nil when the response does not carry it.
func resolvedEntity(r *tg.ContactsResolvedPeer) any {
	switch p := r.Peer.(type) {
	case *tg.PeerUser:
		if user, ok := r.MapUsers().UserToMap()[p.UserID]; ok {
			return user
		}
	case *tg.PeerChat:
		if chat, ok := r.MapChats().ChatToMap()[p.ChatID]; ok {
			return chat
		}
	case *tg.PeerChannel:
		if ch, ok := r.MapChats().ChannelToMap()[p.ChannelID]; ok {
			return ch
		}
	}
	return nil
}

// errResolvedNotPresent reports a response that does not carry the entity its
// r.Peer designates.
var errResolvedNotPresent = errors.New("resolved peer not present in response entities")

// resolvedPeer builds the peer — InputPeer and entity, as the resolver
// returns them — for the canonical resolved peer.
func resolvedPeer(r *tg.ContactsResolvedPeer) (tgclient.Peer, error) {
	switch e := resolvedEntity(r).(type) {
	case *tg.User:
		return tgclient.PeerFromEntity(e)
	case *tg.Chat:
		return tgclient.PeerFromEntity(e)
	case *tg.Channel:
		return tgclient.PeerFromEntity(e)
	}
	return tgclient.Peer{}, errResolvedNotPresent
}

// resolvedPeerInfo returns the canonical resolved peer's bare MTProto ID and
// display title.
func resolvedPeerInfo(r *tg.ContactsResolvedPeer) (id int64, title string, ok bool) {
	switch e := resolvedEntity(r).(type) {
	case *tg.User:
		return e.ID, strings.TrimSpace(e.FirstName + " " + e.LastName), true
	case *tg.Chat:
		return e.ID, e.Title, true
	case *tg.Channel:
		return e.ID, e.Title, true
	}
	return 0, "", false
}
