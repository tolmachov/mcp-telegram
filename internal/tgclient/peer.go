package tgclient

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// ChannelIDPrefix is the offset the Telegram Bot API adds to channel IDs (the
// "-100" prefix). This server speaks MTProto and uses bare positive IDs — the
// same numbers the official Telegram clients show — everywhere. The constant is
// kept only to parse legacy Bot-API "-100…" values still accepted on input.
const ChannelIDPrefix = 1_000_000_000_000

// peerHint pins which peer type to resolve. A bare positive ID carries no hint
// and is probed user → channel → chat; a legacy negative (Bot-API-marked) ID
// normalises to a bare ID plus a hint that limits the probe to a single type.
type peerHint int

const (
	hintNone peerHint = iota
	hintBasicChat
	hintChannel
)

// peerProbe attempts to resolve a bare MTProto ID as one specific peer type.
// It returns (peer, nil) on a match, (nil, nil) when the ID is definitively not
// that type (the caller falls through to the next probe), or (nil, err) for a
// genuine/terminal failure that must abort the whole sweep.
type peerProbe func(ctx context.Context, client *tg.Client, id int64) (tg.InputPeerClass, error)

// ResolvePeer resolves a chat ID to an InputPeerClass, fetching the access_hash
// MTProto requires for users and channels.
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
// Legacy Bot-API "marked" IDs are still accepted: a negative ID is normalised to
// its bare form and pins the probe to a single type (magnitude > 10¹² → channel,
// otherwise basic chat).
func ResolvePeer(ctx context.Context, client *tg.Client, dialogID int64) (tg.InputPeerClass, error) {
	bareID, hint := normalizeDialogID(dialogID)

	var probes []peerProbe
	switch hint {
	case hintChannel:
		probes = []peerProbe{resolveChannel}
	case hintBasicChat:
		probes = []peerProbe{resolveBasicChat}
	default:
		probes = []peerProbe{resolveUser, resolveChannel, resolveBasicChat}
	}

	// Probe each candidate type in order. A match returns immediately; a
	// genuine error aborts the sweep immediately (so a flood-wait or auth
	// failure surfaces as itself, unmisattributed, and we don't fire further
	// live calls into a rate-limit window); only a (nil, nil) "not this type"
	// result falls through to the next probe.
	for _, probe := range probes {
		peer, err := probe(ctx, client, bareID)
		if err != nil {
			return nil, err
		}
		if peer != nil {
			return peer, nil
		}
	}
	return nil, fmt.Errorf("id %d is not a reachable user, chat, or channel; verify it with ResolveUsername (by @handle) or SearchChats (by title)", dialogID)
}

// normalizeDialogID converts a caller-supplied chat ID into a bare MTProto ID
// plus a type hint. Positive IDs pass through untouched with no hint. Negative
// IDs are treated as legacy Bot-API-marked values: magnitudes above the channel
// prefix are channels/supergroups, smaller ones are basic groups.
func normalizeDialogID(dialogID int64) (int64, peerHint) {
	if dialogID >= 0 {
		return dialogID, hintNone
	}
	magnitude := -dialogID
	if magnitude > ChannelIDPrefix {
		return magnitude - ChannelIDPrefix, hintChannel
	}
	return magnitude, hintBasicChat
}

// resolveUser probes users.getUsers. It returns (nil, nil) when the ID is not a
// reachable user, so the caller falls through to the next type. A known user
// without an access_hash is a hard error that aborts the sweep: no shared dialog
// means no valid InputPeerUser can be built, and the specific diagnostic is more
// useful than the generic "not reachable" message the fall-through would yield.
func resolveUser(ctx context.Context, client *tg.Client, id int64) (tg.InputPeerClass, error) {
	users, err := client.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUser{UserID: id}})
	if err != nil {
		return nil, fmt.Errorf("resolving user %d: %w", id, err)
	}
	if len(users) == 0 {
		return nil, nil
	}
	user, ok := users[0].(*tg.User)
	if !ok {
		return nil, nil // *tg.UserEmpty — not a user
	}
	if user.AccessHash == 0 {
		return nil, fmt.Errorf("user %d resolved but missing access_hash; no shared dialog", id)
	}
	return &tg.InputPeerUser{UserID: id, AccessHash: user.AccessHash}, nil
}

// resolveChannel probes channels.getChannels.
//
// CHANNEL_INVALID / PEER_ID_INVALID mean "not a channel" and fall through so the
// caller can try the next type or emit the friendly generic error rather than
// leaking MTProto codes. CHANNEL_PRIVATE is different: the ID *is* a channel that
// this session can't access (e.g. a public channel it hasn't joined, exactly the
// access_hash==0 case here), so it returns an actionable error pointing at
// ResolveUsername instead of pretending the channel doesn't exist.
func resolveChannel(ctx context.Context, client *tg.Client, id int64) (tg.InputPeerClass, error) {
	channels, err := client.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: id}})
	if err != nil {
		switch {
		case tgerr.Is(err, "CHANNEL_INVALID", "PEER_ID_INVALID"):
			return nil, nil
		case tgerr.Is(err, "CHANNEL_PRIVATE"):
			return nil, fmt.Errorf("channel %d exists but is inaccessible from this account; resolve it by @username with ResolveUsername (and join it if needed)", id)
		default:
			return nil, fmt.Errorf("resolving channel %d: %w", id, err)
		}
	}
	chats, ok := channels.(*tg.MessagesChats)
	if !ok || len(chats.Chats) == 0 {
		return nil, nil
	}
	channel, ok := chats.Chats[0].(*tg.Channel)
	if !ok {
		return nil, nil
	}
	return &tg.InputPeerChannel{ChannelID: channel.ID, AccessHash: channel.AccessHash}, nil
}

// resolveBasicChat probes messages.getChats for a legacy basic group, which
// needs no access_hash. Returns (nil, nil) when the ID is not a basic chat.
func resolveBasicChat(ctx context.Context, client *tg.Client, id int64) (tg.InputPeerClass, error) {
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
			return nil, nil
		}
		return nil, fmt.Errorf("resolving basic chat %d: %w", id, err)
	}
	msgChats, ok := chats.(*tg.MessagesChats)
	if !ok || len(msgChats.Chats) == 0 {
		return nil, nil
	}
	if _, ok := msgChats.Chats[0].(*tg.Chat); !ok {
		return nil, nil // chatEmpty / not a basic chat
	}
	return &tg.InputPeerChat{ChatID: id}, nil
}
