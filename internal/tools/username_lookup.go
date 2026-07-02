package tools

import (
	"context"
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

// resolvedInputPeer builds the InputPeer for the canonical resolved peer
// (r.Peer), looking up the access hash among the response entities.
func resolvedInputPeer(r *tg.ContactsResolvedPeer) (tg.InputPeerClass, error) {
	switch p := r.Peer.(type) {
	case *tg.PeerUser:
		for _, u := range r.Users {
			if user, ok := u.(*tg.User); ok && user.ID == p.UserID {
				if user.AccessHash == 0 {
					return nil, fmt.Errorf("resolved but missing access hash (no shared dialog)")
				}
				return &tg.InputPeerUser{UserID: user.ID, AccessHash: user.AccessHash}, nil
			}
		}
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: p.ChatID}, nil
	case *tg.PeerChannel:
		for _, c := range r.Chats {
			if ch, ok := c.(*tg.Channel); ok && ch.ID == p.ChannelID {
				if ch.AccessHash == 0 {
					return nil, fmt.Errorf("resolved but missing access hash (no shared dialog)")
				}
				return &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}, nil
			}
		}
	}
	return nil, fmt.Errorf("resolved peer %T not present in response entities", r.Peer)
}

// resolvedPeerInfo returns the canonical resolved peer's bare MTProto ID and
// display title.
func resolvedPeerInfo(r *tg.ContactsResolvedPeer) (id int64, title string, ok bool) {
	switch p := r.Peer.(type) {
	case *tg.PeerUser:
		for _, u := range r.Users {
			if user, uok := u.(*tg.User); uok && user.ID == p.UserID {
				return user.ID, strings.TrimSpace(user.FirstName + " " + user.LastName), true
			}
		}
	case *tg.PeerChat:
		for _, c := range r.Chats {
			if chat, cok := c.(*tg.Chat); cok && chat.ID == p.ChatID {
				return chat.ID, chat.Title, true
			}
		}
	case *tg.PeerChannel:
		for _, c := range r.Chats {
			if ch, cok := c.(*tg.Channel); cok && ch.ID == p.ChannelID {
				return ch.ID, ch.Title, true
			}
		}
	}
	return 0, "", false
}

// resolvedChannel returns the canonical resolved peer as a channel/supergroup,
// erroring for users and basic groups (which have no InputChannel form). Used
// by JoinChat/LeaveChat, which act only on channels.
func resolvedChannel(r *tg.ContactsResolvedPeer) (*tg.InputChannel, *tg.Channel, error) {
	p, ok := r.Peer.(*tg.PeerChannel)
	if !ok {
		return nil, nil, fmt.Errorf("not a channel or supergroup")
	}
	for _, c := range r.Chats {
		if ch, cok := c.(*tg.Channel); cok && ch.ID == p.ChannelID {
			if ch.AccessHash == 0 {
				return nil, nil, fmt.Errorf("resolved but missing access hash (no shared dialog)")
			}
			return &tg.InputChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}, ch, nil
		}
	}
	return nil, nil, fmt.Errorf("resolved channel not present in response entities")
}
