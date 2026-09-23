package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gotd/td/tg"
)

// This file is the single username-resolution seam for the tool handlers.
// Several tools (AddChatsToFolder, ResolveMessageLink, JoinChat/LeaveChat) turn
// a public @username into a peer, and each used to hand-roll the
// contacts.resolveUsername call and then guess which returned entity was the
// answer — one taking the first user, another the first chat. Because a single
// response can carry several entities (a bot inside a chat, a linked discussion
// group), those guesses could disagree and hand the same @username a different
// chat_id in different tools. The accessors below all key off r.Peer — the
// entity Telegram itself designates as the resolution — so every caller agrees.

// resolvePublicUsername normalizes a public username (with or without a leading
// @), rejects empty input, and calls contacts.resolveUsername.
func resolvePublicUsername(ctx context.Context, client *tg.Client, username string) (*tg.ContactsResolvedPeer, error) {
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	if username == "" {
		return nil, fmt.Errorf("empty username")
	}
	resolved, err := client.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return nil, fmt.Errorf("resolving @%s: %w", username, err)
	}
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

// errNoSharedDialog reports a resolved user or channel without an access hash.
var errNoSharedDialog = errors.New("resolved but missing access hash (no shared dialog)")

// resolvedInputPeer builds the InputPeer for the canonical resolved peer.
func resolvedInputPeer(r *tg.ContactsResolvedPeer) (tg.InputPeerClass, error) {
	switch e := resolvedEntity(r).(type) {
	case *tg.User:
		if e.AccessHash == 0 {
			return nil, errNoSharedDialog
		}
		return &tg.InputPeerUser{UserID: e.ID, AccessHash: e.AccessHash}, nil
	case *tg.Chat:
		return &tg.InputPeerChat{ChatID: e.ID}, nil
	case *tg.Channel:
		if e.AccessHash == 0 {
			return nil, errNoSharedDialog
		}
		return &tg.InputPeerChannel{ChannelID: e.ID, AccessHash: e.AccessHash}, nil
	}
	return nil, errResolvedNotPresent
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

// resolvedChannel returns the canonical resolved peer as a channel/supergroup,
// erroring for users and basic groups (which have no InputChannel form). Used
// by JoinChat, which acts only on channels.
func resolvedChannel(r *tg.ContactsResolvedPeer) (*tg.InputChannel, *tg.Channel, error) {
	switch e := resolvedEntity(r).(type) {
	case *tg.Channel:
		if e.AccessHash == 0 {
			return nil, nil, errNoSharedDialog
		}
		return &tg.InputChannel{ChannelID: e.ID, AccessHash: e.AccessHash}, e, nil
	case nil:
		return nil, nil, errResolvedNotPresent
	}
	return nil, nil, fmt.Errorf("not a channel or supergroup")
}
