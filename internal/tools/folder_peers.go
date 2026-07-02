package tools

import (
	"github.com/gotd/td/tg"
)

// This file holds the pure peer-set algebra and folder-ID helpers used by the
// folder (dialog filter) tools. They are free of context/API concerns —
// separated from the handlers in folders.go so the operation logic there reads
// without wading through set arithmetic.

// peerBareID extracts the peer kind and bare MTProto ID from an InputPeer —
// the bare ID the official Telegram clients display (0 for self/Saved Messages).
// ok is false for peer forms that carry no stable bare ID (empty / from-message).
func peerBareID(p tg.InputPeerClass) (kind string, id int64, ok bool) {
	switch v := p.(type) {
	case *tg.InputPeerUser:
		return "user", v.UserID, true
	case *tg.InputPeerChat:
		return "chat", v.ChatID, true
	case *tg.InputPeerChannel:
		return "channel", v.ChannelID, true
	case *tg.InputPeerSelf:
		return "self", 0, true
	default:
		return "", 0, false
	}
}

// samePeer reports whether two InputPeers reference the same dialog, comparing
// by kind and bare ID only. The access_hash is deliberately ignored: the same
// peer can carry different hashes across calls, but kind+ID is stable.
func samePeer(a, b tg.InputPeerClass) bool {
	ka, ia, oka := peerBareID(a)
	kb, ib, okb := peerBareID(b)
	if !oka || !okb {
		return false
	}
	return ka == kb && ia == ib
}

// containsPeer reports whether peers already includes target (by samePeer).
func containsPeer(peers []tg.InputPeerClass, target tg.InputPeerClass) bool {
	for _, p := range peers {
		if samePeer(p, target) {
			return true
		}
	}
	return false
}

// removePeer returns peers with every entry matching target removed.
func removePeer(peers []tg.InputPeerClass, target tg.InputPeerClass) []tg.InputPeerClass {
	out := make([]tg.InputPeerClass, 0, len(peers))
	for _, p := range peers {
		if !samePeer(p, target) {
			out = append(out, p)
		}
	}
	return out
}

// peerBareIDs maps a peer slice to its bare IDs, skipping peers without one.
func peerBareIDs(peers []tg.InputPeerClass) []int64 {
	ids := make([]int64, 0, len(peers))
	for _, p := range peers {
		if _, id, ok := peerBareID(p); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// nextFolderID returns the smallest free folder ID >= minCustomFolderID.
func nextFolderID(existing []int) int {
	used := make(map[int]bool, len(existing))
	for _, id := range existing {
		used[id] = true
	}
	id := minCustomFolderID
	for used[id] {
		id++
	}
	return id
}

// folderIDs collects the IDs of every folder (standard and shared) in the list.
func folderIDs(filters *tg.MessagesDialogFilters) []int {
	ids := make([]int, 0, len(filters.Filters))
	for _, f := range filters.Filters {
		switch df := f.(type) {
		case *tg.DialogFilter:
			ids = append(ids, df.ID)
		case *tg.DialogFilterChatlist:
			ids = append(ids, df.ID)
		}
	}
	return ids
}

// filterHasInclusion reports whether a filter would include any dialog at all.
// Telegram rejects a folder with no peers and no category flags
// (FILTER_INCLUDE_EMPTY), so callers check this before writing.
func filterHasInclusion(f *tg.DialogFilter) bool {
	return len(f.IncludePeers) > 0 || len(f.PinnedPeers) > 0 ||
		f.Contacts || f.NonContacts || f.Groups || f.Broadcasts || f.Bots
}
