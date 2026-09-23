package tgclient

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// Peer is a chat ID resolved to the InputPeer MTProto calls take, together
// with the entity it was resolved from: User for a user, otherwise Chat — a
// *tg.Chat for a basic group or a *tg.Channel for a channel or supergroup.
type Peer struct {
	Input tg.InputPeerClass
	User  *tg.User
	Chat  tg.ChatClass
}

// ID returns the bare chat ID the peer was resolved from, the key the
// Resolver caches it under.
func (p Peer) ID() int64 {
	switch in := p.Input.(type) {
	case *tg.InputPeerUser:
		return in.UserID
	case *tg.InputPeerChat:
		return in.ChatID
	case *tg.InputPeerChannel:
		return in.ChannelID
	default:
		return 0
	}
}

// peerProbe attempts to resolve a bare MTProto ID as one specific peer type.
// It returns (peer, true, nil) on a match, (_, false, nil) when the ID is
// definitively not that type (the caller falls through to the next probe), or
// (_, false, err) for a genuine/terminal failure that must abort the whole
// sweep.
type peerProbe func(ctx context.Context, client *tg.Client, id int64) (Peer, bool, error)

// PeerFromEntity builds the Peer for an entity Telegram returned: a user, a
// basic chat or a channel. MTProto addresses a user or a channel by its access
// hash, so one Telegram returned without it — the account shares no dialog
// with it — cannot be addressed and fails with ErrUnresolvablePeer.
func PeerFromEntity[E *tg.User | *tg.Chat | *tg.Channel](entity E) (Peer, error) {
	switch e := any(entity).(type) {
	case *tg.User:
		if e.AccessHash == 0 {
			return Peer{}, unresolvable(e.ID, "Telegram returned the user without an access hash, so this account shares no dialog with them")
		}
		return Peer{Input: &tg.InputPeerUser{UserID: e.ID, AccessHash: e.AccessHash}, User: e}, nil
	case *tg.Channel:
		if e.AccessHash == 0 {
			return Peer{}, unresolvable(e.ID, "Telegram returned the channel without an access hash, so this account shares no dialog with it")
		}
		return Peer{Input: &tg.InputPeerChannel{ChannelID: e.ID, AccessHash: e.AccessHash}, Chat: e}, nil
	default:
		chat := any(entity).(*tg.Chat)
		return Peer{Input: &tg.InputPeerChat{ChatID: chat.ID}, Chat: chat}, nil
	}
}

// resolvePeer resolves a chat ID to a Peer, fetching the access_hash MTProto
// requires for users and channels. Resolver is its only caller: it caches the
// result and owns the stale-hash retry.
//
// The canonical input is a bare MTProto ID: the positive number the official
// Telegram clients display (e.g. 1555091578 for a channel). Users, basic chats
// and channels all live in the positive ID space, so the peer type is found by
// probing users.getUsers → channels.getChannels → messages.getChats; the first
// reachable match wins. Channels are probed before basic chats because they are
// the common modern case, which also keeps channel resolution off the legacy
// messages.getChats path entirely. Users take priority on the astronomically
// unlikely numeric collision.
//
// Non-positive IDs, including Bot-API "-100…" marked IDs, are rejected. A
// failure that is down to the ID itself is ErrUnresolvablePeer; any other one
// is the probe's own error.
func resolvePeer(ctx context.Context, client *tg.Client, dialogID int64) (Peer, error) {
	if dialogID <= 0 {
		return Peer{}, unresolvable(dialogID, "pass the positive ID Telegram clients show (Bot-API \"-100…\" IDs are not accepted)")
	}

	// Probe each candidate type in order. A match returns immediately; a
	// genuine error aborts the sweep immediately (so a flood-wait or auth
	// failure surfaces as itself, unmisattributed, and we don't fire further
	// live calls into a rate-limit window); only a "not this type" result
	// (ok false, nil error) falls through to the next probe.
	for _, probe := range []peerProbe{resolveUser, resolveChannel, resolveBasicChat} {
		peer, ok, err := probe(ctx, client, dialogID)
		if err != nil {
			return Peer{}, err
		}
		if ok {
			return peer, nil
		}
	}
	return Peer{}, unresolvable(dialogID, "it is not a reachable user, chat, or channel; verify it with ResolveUsername (by @handle) or SearchChats (by title)")
}

// resolveUser probes users.getUsers. It reports no match when the ID is not a
// reachable user, so the caller falls through to the next type. A known user
// without an access_hash is a hard error that aborts the sweep: no shared dialog
// means no valid InputPeerUser can be built, and the specific diagnostic is more
// useful than the generic "not reachable" message the fall-through would yield.
func resolveUser(ctx context.Context, client *tg.Client, id int64) (Peer, bool, error) {
	users, err := client.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUser{UserID: id}})
	if err != nil {
		return Peer{}, false, fmt.Errorf("resolving user %d: %w", id, err)
	}
	if len(users) == 0 {
		return Peer{}, false, nil
	}
	user, ok := users[0].(*tg.User)
	if !ok {
		return Peer{}, false, nil // *tg.UserEmpty — not a user
	}
	peer, err := PeerFromEntity(user)
	return peer, err == nil, err
}

// resolveChannel probes channels.getChannels.
//
// CHANNEL_INVALID / PEER_ID_INVALID mean "not a channel" and fall through so the
// caller can try the next type or emit the friendly generic error rather than
// leaking MTProto codes. CHANNEL_PRIVATE is different: the ID *is* a channel that
// this session can't access (e.g. a public channel it hasn't joined), so it
// returns an actionable error pointing at ResolveUsername instead of pretending
// the channel doesn't exist.
func resolveChannel(ctx context.Context, client *tg.Client, id int64) (Peer, bool, error) {
	channels, err := client.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: id}})
	if err != nil {
		switch {
		case tgerr.Is(err, "CHANNEL_INVALID", "PEER_ID_INVALID"):
			return Peer{}, false, nil
		case tgerr.Is(err, "CHANNEL_PRIVATE"):
			return Peer{}, false, unresolvable(id, "the channel exists but is inaccessible from this account; resolve it by @username with ResolveUsername (and join it if needed)")
		default:
			return Peer{}, false, fmt.Errorf("resolving channel %d: %w", id, err)
		}
	}
	chats, ok := channels.(*tg.MessagesChats)
	if !ok || len(chats.Chats) == 0 {
		return Peer{}, false, nil
	}
	channel, ok := chats.Chats[0].(*tg.Channel)
	if !ok {
		return Peer{}, false, nil
	}
	peer, err := PeerFromEntity(channel)
	return peer, err == nil, err
}

// resolveBasicChat probes messages.getChats for a legacy basic group, which
// needs no access_hash. Reports no match when the ID is not a basic chat.
func resolveBasicChat(ctx context.Context, client *tg.Client, id int64) (Peer, bool, error) {
	chats, err := client.MessagesGetChats(ctx, []int64{id})
	if err != nil {
		// CHAT_ID_INVALID / PEER_ID_INVALID mean "not a basic chat" — fall
		// through (like resolveChannel does for CHANNEL_INVALID) so the caller
		// emits the friendly generic error pointing at ResolveUsername /
		// SearchChats instead of leaking the raw MTProto code. This is the
		// common outcome for a bare channel/supergroup ID with no known
		// access_hash: it isn't a basic chat, so the actionable answer is
		// "resolve it by @username", not "CHAT_ID_INVALID".
		if tgerr.Is(err, "CHAT_ID_INVALID", "PEER_ID_INVALID") {
			return Peer{}, false, nil
		}
		return Peer{}, false, fmt.Errorf("resolving basic chat %d: %w", id, err)
	}
	msgChats, ok := chats.(*tg.MessagesChats)
	if !ok || len(msgChats.Chats) == 0 {
		return Peer{}, false, nil
	}
	chat, ok := msgChats.Chats[0].(*tg.Chat)
	if !ok {
		return Peer{}, false, nil // chatEmpty / not a basic chat
	}
	peer, err := PeerFromEntity(chat)
	return peer, err == nil, err
}
