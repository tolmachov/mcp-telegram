package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// Folder (dialog filter) constraints enforced before hitting the Telegram API.
const (
	// maxFolderTitleRunes is Telegram's limit on a folder name (12 runes).
	maxFolderTitleRunes = 12
	// minCustomFolderID is the lowest ID a user folder may take: 0 is the
	// built-in "All chats" view and 1 is the Archive (per Telegram convention),
	// so custom folders start at 2.
	minCustomFolderID = 2
)

// Folder kind/status strings reported in tool output.
const (
	folderKindStandard  = "folder"
	folderKindShared    = "shared_folder"
	folderStatusDeleted = "deleted"
)

// FolderSkippedChat reports a chat reference that could not be applied to a
// folder operation, with a human-readable reason the model can act on.
type FolderSkippedChat struct {
	Chat   string `json:"chat"`
	Reason string `json:"reason"`
}

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

// isFatalResolveErr reports whether a peer-resolution error is systemic — a
// rate limit, a cancelled/timed-out context, or a dead session — rather than a
// problem with one chat reference. Fatal errors must abort a batch instead of
// being demoted to a per-chat skip: continuing would hammer a rate limit or a
// dead session, and a "skipped" entry would hide a real failure behind a
// successful-looking result.
func isFatalResolveErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if _, ok := tgerr.AsFloodWait(err); ok {
		return true
	}
	return tgerr.Is(err, "AUTH_KEY_UNREGISTERED", "SESSION_REVOKED", "SESSION_EXPIRED", "USER_DEACTIVATED")
}

// resolvePeerRef resolves a folder chat reference (a public @username or a
// numeric chat ID) to an InputPeer. Unlike resolveChannelByUsername it supports
// users, channels and basic groups, since folders can hold any of them.
//
// A per-chat problem (typo, no shared dialog, invite link) is returned as a
// non-empty reason string so batch callers can skip that one and keep going. A
// systemic failure (rate limit, cancellation, dead session) is returned as a
// non-nil fatal error so callers abort the whole batch instead of masking it.
func resolvePeerRef(ctx context.Context, client *tg.Client, ref string) (peer tg.InputPeerClass, reason string, fatal error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, "empty chat reference", nil
	}
	kind, value := classifyChatRef(ref)
	switch kind {
	case chatRefInvite:
		return nil, "is an invite link; join the chat first with JoinChat, then add it by @username or numeric ID", nil
	case chatRefUsername:
		peer, err := resolveInputPeerByUsername(ctx, client, value)
		if err != nil {
			if isFatalResolveErr(err) {
				return nil, "", fmt.Errorf("resolving @%s: %w", value, err)
			}
			return nil, fmt.Sprintf("could not resolve @%s: %v", value, err), nil
		}
		return peer, "", nil
	default: // chatRefID
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, fmt.Sprintf("invalid chat reference %q: expected a public @username or a numeric chat ID", ref), nil
		}
		peer, err := tgclient.ResolvePeer(ctx, client, id)
		if err != nil {
			if isFatalResolveErr(err) {
				return nil, "", fmt.Errorf("resolving chat %d: %w", id, err)
			}
			return nil, fmt.Sprintf("could not resolve chat %d: %v", id, err), nil
		}
		return peer, "", nil
	}
}

// resolveChatRefs resolves a batch of chat references into peers, collecting
// per-chat skips. It aborts with a non-nil fatal error on a cancelled context
// or the first systemic resolution failure (see isFatalResolveErr), so the
// caller surfaces it rather than masking it as a successful no-op. Peers that
// resolve to a form without a bare ID are skipped, so every returned peer is
// safe to identify with peerBareID.
func resolveChatRefs(ctx context.Context, client *tg.Client, refs []string) (peers []tg.InputPeerClass, skipped []FolderSkippedChat, fatal error) {
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			return nil, skipped, err
		}
		peer, reason, ferr := resolvePeerRef(ctx, client, ref)
		if ferr != nil {
			return nil, skipped, ferr
		}
		if reason != "" {
			skipped = append(skipped, FolderSkippedChat{Chat: ref, Reason: reason})
			continue
		}
		if _, _, ok := peerBareID(peer); !ok {
			skipped = append(skipped, FolderSkippedChat{Chat: ref, Reason: "resolved to an unsupported peer form"})
			continue
		}
		peers = append(peers, peer)
	}
	return peers, skipped, nil
}

// applyAdditions adds each peer to the folder's include list, de-duplicating
// against both the include and pinned lists (a pinned chat is already in the
// folder), and drops each added peer from the exclude list so it isn't both
// included and excluded. It returns the bare IDs actually added and those
// already present. Every peer must carry a bare ID (guaranteed by resolveChatRefs).
func applyAdditions(filter *tg.DialogFilter, peers []tg.InputPeerClass) (added, alreadyPresent []int64) {
	for _, peer := range peers {
		_, id, _ := peerBareID(peer)
		if containsPeer(filter.IncludePeers, peer) || containsPeer(filter.PinnedPeers, peer) {
			alreadyPresent = append(alreadyPresent, id)
			continue
		}
		filter.IncludePeers = append(filter.IncludePeers, peer)
		filter.ExcludePeers = removePeer(filter.ExcludePeers, peer)
		added = append(added, id)
	}
	return added, alreadyPresent
}

// applyRemovals drops each peer from the folder's include and pinned lists (a
// chat may be pinned, included, or both). It returns the bare IDs actually
// removed and those that weren't in the folder.
func applyRemovals(filter *tg.DialogFilter, peers []tg.InputPeerClass) (removed, notPresent []int64) {
	for _, peer := range peers {
		_, id, _ := peerBareID(peer)
		if !containsPeer(filter.IncludePeers, peer) && !containsPeer(filter.PinnedPeers, peer) {
			notPresent = append(notPresent, id)
			continue
		}
		filter.IncludePeers = removePeer(filter.IncludePeers, peer)
		filter.PinnedPeers = removePeer(filter.PinnedPeers, peer)
		removed = append(removed, id)
	}
	return removed, notPresent
}

// formatSkipped renders skipped chats as "chat (reason); chat (reason)" for
// embedding in an error message when an entire batch failed to resolve.
func formatSkipped(skipped []FolderSkippedChat) string {
	parts := make([]string, 0, len(skipped))
	for _, s := range skipped {
		parts = append(parts, fmt.Sprintf("%s (%s)", s.Chat, s.Reason))
	}
	return strings.Join(parts, "; ")
}

// resolveInputPeerByUsername resolves a public @username to an InputPeer,
// supporting users, channels/supergroups and basic groups. The first user wins,
// then the first chat/channel, mirroring how ResolveUsername reports entities.
func resolveInputPeerByUsername(ctx context.Context, client *tg.Client, username string) (tg.InputPeerClass, error) {
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	if username == "" {
		return nil, fmt.Errorf("empty username")
	}
	resolved, err := client.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return nil, err
	}
	for _, u := range resolved.Users {
		user, ok := u.(*tg.User)
		if !ok {
			continue
		}
		if user.AccessHash == 0 {
			return nil, fmt.Errorf("resolved but missing access hash (no shared dialog)")
		}
		return &tg.InputPeerUser{UserID: user.ID, AccessHash: user.AccessHash}, nil
	}
	for _, c := range resolved.Chats {
		switch chat := c.(type) {
		case *tg.Channel:
			return &tg.InputPeerChannel{ChannelID: chat.ID, AccessHash: chat.AccessHash}, nil
		case *tg.Chat:
			return &tg.InputPeerChat{ChatID: chat.ID}, nil
		}
	}
	return nil, fmt.Errorf("did not resolve to a user, group, or channel")
}

// findEditableFolder fetches all folders and returns the standard DialogFilter
// with the given ID, ready for an in-place edit. It returns an error result
// when the ID is unknown or names a shared (imported chatlist) folder, whose
// chats cannot be edited through updateDialogFilter.
func findEditableFolder(ctx context.Context, client *tg.Client, folderID int) (*tg.DialogFilter, *mcp.CallToolResult) {
	filters, err := client.MessagesGetDialogFilters(ctx)
	if err != nil {
		return nil, errResult(fmt.Sprintf("Failed to read folders: %v", err))
	}
	for _, f := range filters.Filters {
		switch df := f.(type) {
		case *tg.DialogFilter:
			if df.ID == folderID {
				return df, nil
			}
		case *tg.DialogFilterChatlist:
			if df.ID == folderID {
				return nil, errResult(fmt.Sprintf("Folder %d is a shared/imported folder; its chats can't be edited here. Manage it from the Telegram app instead.", folderID))
			}
		}
	}
	return nil, errResult(fmt.Sprintf("No folder with ID %d. List your folders with GetFolders to find the right ID.", folderID))
}

// ---------------------------------------------------------------------------
// GetFolders
// ---------------------------------------------------------------------------

// GetFoldersHandler handles the GetFolders tool.
type GetFoldersHandler struct {
	client *tg.Client
}

// NewGetFoldersHandler creates a new GetFoldersHandler.
func NewGetFoldersHandler(client *tg.Client) *GetFoldersHandler {
	return &GetFoldersHandler{client: client}
}

// GetFoldersInput is the (empty) input for the GetFolders tool.
type GetFoldersInput struct{}

// FolderFlags describes the category-based include/exclude rules of a folder.
type FolderFlags struct {
	Groups          bool `json:"groups,omitempty"`
	Channels        bool `json:"channels,omitempty"`
	Bots            bool `json:"bots,omitempty"`
	Contacts        bool `json:"contacts,omitempty"`
	NonContacts     bool `json:"non_contacts,omitempty"`
	ExcludeMuted    bool `json:"exclude_muted,omitempty"`
	ExcludeRead     bool `json:"exclude_read,omitempty"`
	ExcludeArchived bool `json:"exclude_archived,omitempty"`
}

// FolderInfo is a single folder as reported by GetFolders. Peer references are
// bare MTProto IDs; resolve titles with GetChatInfo if needed. Flags is nil for
// shared folders, where category flags don't apply.
type FolderInfo struct {
	ID         int          `json:"id"`
	Title      string       `json:"title"`
	Kind       string       `json:"kind"` // folderKindStandard | folderKindShared
	Flags      *FolderFlags `json:"flags,omitempty"`
	IncludeIDs []int64      `json:"include_ids,omitempty"`
	ExcludeIDs []int64      `json:"exclude_ids,omitempty"`
	PinnedIDs  []int64      `json:"pinned_ids,omitempty"`
}

// mapDialogFilter converts a Telegram dialog filter to a FolderInfo for output.
// ok is false for the built-in "All chats" default view (*tg.DialogFilterDefault),
// which has no editable representation and is omitted from the listing.
func mapDialogFilter(f tg.DialogFilterClass) (FolderInfo, bool) {
	switch df := f.(type) {
	case *tg.DialogFilter:
		return FolderInfo{
			ID:    df.ID,
			Title: df.Title.Text,
			Kind:  folderKindStandard,
			Flags: &FolderFlags{
				Groups:          df.Groups,
				Channels:        df.Broadcasts,
				Bots:            df.Bots,
				Contacts:        df.Contacts,
				NonContacts:     df.NonContacts,
				ExcludeMuted:    df.ExcludeMuted,
				ExcludeRead:     df.ExcludeRead,
				ExcludeArchived: df.ExcludeArchived,
			},
			IncludeIDs: peerBareIDs(df.IncludePeers),
			ExcludeIDs: peerBareIDs(df.ExcludePeers),
			PinnedIDs:  peerBareIDs(df.PinnedPeers),
		}, true
	case *tg.DialogFilterChatlist:
		return FolderInfo{
			ID:         df.ID,
			Title:      df.Title.Text,
			Kind:       folderKindShared,
			IncludeIDs: peerBareIDs(df.IncludePeers),
			PinnedIDs:  peerBareIDs(df.PinnedPeers),
		}, true
	default:
		return FolderInfo{}, false
	}
}

// GetFoldersResult is the typed output of GetFolders.
type GetFoldersResult struct {
	Folders []FolderInfo `json:"folders"`
}

// Register adds the GetFolders tool to the MCP server.
func (h *GetFoldersHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "GetFolders",
		Description: "List your Telegram chat folders (dialog filters) with their numeric ID, title, category flags, and the bare IDs of included/excluded/pinned chats. Call this first to find the folder_id needed by AddChatsToFolder, RemoveChatsFromFolder, and DeleteFolder. Shared/imported folders are reported with kind \"shared_folder\" and cannot have their chats edited here.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *GetFoldersHandler) handle(ctx context.Context, _ *mcp.CallToolRequest, _ GetFoldersInput) (*mcp.CallToolResult, *GetFoldersResult, error) {
	filters, err := h.client.MessagesGetDialogFilters(ctx)
	if err != nil {
		return errResult(fmt.Sprintf("Failed to get folders: %v", err)), nil, nil
	}
	out := &GetFoldersResult{Folders: make([]FolderInfo, 0, len(filters.Filters))}
	for _, f := range filters.Filters {
		if info, ok := mapDialogFilter(f); ok {
			out.Folders = append(out.Folders, info)
		}
	}
	return nil, out, nil
}

// ---------------------------------------------------------------------------
// CreateFolder
// ---------------------------------------------------------------------------

// CreateFolderHandler handles the CreateFolder tool.
type CreateFolderHandler struct {
	client *tg.Client
}

// NewCreateFolderHandler creates a new CreateFolderHandler.
func NewCreateFolderHandler(client *tg.Client) *CreateFolderHandler {
	return &CreateFolderHandler{client: client}
}

// CreateFolderInput is the input for the CreateFolder tool.
type CreateFolderInput struct {
	Title              string   `json:"title" jsonschema:"Folder name to display (max 12 characters)"`
	Chats              []string `json:"chats,omitempty" jsonschema:"Chats to include initially: each a public @username or a numeric chat ID"`
	IncludeContacts    bool     `json:"include_contacts,omitempty" jsonschema:"Include all contacts in the folder"`
	IncludeNonContacts bool     `json:"include_non_contacts,omitempty" jsonschema:"Include all non-contacts in the folder"`
	IncludeGroups      bool     `json:"include_groups,omitempty" jsonschema:"Include all groups in the folder"`
	IncludeChannels    bool     `json:"include_channels,omitempty" jsonschema:"Include all broadcast channels in the folder"`
	IncludeBots        bool     `json:"include_bots,omitempty" jsonschema:"Include all bots in the folder"`
	ExcludeMuted       bool     `json:"exclude_muted,omitempty" jsonschema:"Exclude muted chats from the folder"`
	ExcludeRead        bool     `json:"exclude_read,omitempty" jsonschema:"Exclude already-read chats from the folder"`
	ExcludeArchived    bool     `json:"exclude_archived,omitempty" jsonschema:"Exclude archived chats from the folder"`
}

// CreateFolderResult is the typed output of CreateFolder.
type CreateFolderResult struct {
	FolderID      int                 `json:"folder_id"`
	Title         string              `json:"title"`
	IncludedCount int                 `json:"included_count"`
	Skipped       []FolderSkippedChat `json:"skipped,omitempty"`
}

// hasCategoryInclude reports whether any include_* category flag is set.
func (in CreateFolderInput) hasCategoryInclude() bool {
	return in.IncludeContacts || in.IncludeNonContacts || in.IncludeGroups ||
		in.IncludeChannels || in.IncludeBots
}

// Register adds the CreateFolder tool to the MCP server.
func (h *CreateFolderHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "CreateFolder",
		Description: "Create a new Telegram chat folder (dialog filter). Provide a title (max 12 characters) and at least one source of chats: an explicit list of @usernames / numeric chat IDs, and/or category flags like include_groups or include_channels. The folder ID is assigned automatically. Add or remove chats later with AddChatsToFolder / RemoveChatsFromFolder.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *CreateFolderHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in CreateFolderInput) (*mcp.CallToolResult, *CreateFolderResult, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return errResult("title is required: a short folder name (max 12 characters)."), nil, nil
	}
	if utf8.RuneCountInString(title) > maxFolderTitleRunes {
		return errResult(fmt.Sprintf("title %q is too long: max %d characters.", title, maxFolderTitleRunes)), nil, nil
	}
	if len(in.Chats) == 0 && !in.hasCategoryInclude() {
		return errResult("a folder needs at least one chat to include or one include_* category flag (e.g. include_groups). Telegram rejects empty folders."), nil, nil
	}

	filters, err := h.client.MessagesGetDialogFilters(ctx)
	if err != nil {
		return errResult(fmt.Sprintf("Failed to read existing folders: %v", err)), nil, nil
	}
	id := nextFolderID(folderIDs(filters))

	peers, skipped, ferr := resolveChatRefs(ctx, h.client, in.Chats)
	if ferr != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "CreateFolder", map[string]any{"title": title, "error": ferr.Error()})
		return errResult(fmt.Sprintf("Aborted creating folder %q: %v. Retry once the condition clears.", title, ferr)), nil, nil
	}
	includePeers := make([]tg.InputPeerClass, 0, len(peers))
	for _, peer := range peers {
		if !containsPeer(includePeers, peer) {
			includePeers = append(includePeers, peer)
		}
	}
	if len(includePeers) == 0 && !in.hasCategoryInclude() {
		return errResult("none of the provided chats could be resolved and no include_* flag was set, so the folder would be empty. Check the chat references (try ResolveUsername or SearchChats) and retry."), nil, nil
	}

	filter := &tg.DialogFilter{
		ID:              id,
		Title:           tg.TextWithEntities{Text: title},
		Contacts:        in.IncludeContacts,
		NonContacts:     in.IncludeNonContacts,
		Groups:          in.IncludeGroups,
		Broadcasts:      in.IncludeChannels,
		Bots:            in.IncludeBots,
		ExcludeMuted:    in.ExcludeMuted,
		ExcludeRead:     in.ExcludeRead,
		ExcludeArchived: in.ExcludeArchived,
		IncludePeers:    includePeers,
	}
	updReq := &tg.MessagesUpdateDialogFilterRequest{ID: id}
	updReq.SetFilter(filter)
	if _, err := h.client.MessagesUpdateDialogFilter(ctx, updReq); err != nil {
		if tgerr.Is(err, "DIALOG_FILTERS_TOO_MUCH") {
			return errResult("You've reached the maximum number of folders. Delete one with DeleteFolder first, or a Telegram Premium subscription raises the limit."), nil, nil
		}
		mcpLog(ctx, req.Session, logLevelWarning, "CreateFolder", map[string]any{"title": title, "error": err.Error()})
		return errResult(fmt.Sprintf("Failed to create folder %q: %v", title, err)), nil, nil
	}
	return nil, &CreateFolderResult{
		FolderID:      id,
		Title:         title,
		IncludedCount: len(includePeers),
		Skipped:       skipped,
	}, nil
}

// ---------------------------------------------------------------------------
// DeleteFolder
// ---------------------------------------------------------------------------

// DeleteFolderHandler handles the DeleteFolder tool.
type DeleteFolderHandler struct {
	client *tg.Client
}

// NewDeleteFolderHandler creates a new DeleteFolderHandler.
func NewDeleteFolderHandler(client *tg.Client) *DeleteFolderHandler {
	return &DeleteFolderHandler{client: client}
}

// DeleteFolderInput is the input for the DeleteFolder tool.
type DeleteFolderInput struct {
	FolderID int `json:"folder_id" jsonschema:"Numeric folder ID to delete (from GetFolders)"`
}

// DeleteFolderResult is the typed output of DeleteFolder.
type DeleteFolderResult struct {
	Status   string `json:"status"` // "deleted"
	FolderID int    `json:"folder_id"`
}

// Register adds the DeleteFolder tool to the MCP server.
func (h *DeleteFolderHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "DeleteFolder",
		Description: "Delete a Telegram chat folder (dialog filter) by its numeric ID. This removes only the folder view — your chats and their messages are untouched. Find the folder_id with GetFolders. The host may ask you to confirm.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptrTrue(), OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *DeleteFolderHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in DeleteFolderInput) (*mcp.CallToolResult, *DeleteFolderResult, error) {
	if in.FolderID <= 0 {
		return errResult("folder_id is required: a positive folder ID from GetFolders."), nil, nil
	}

	filters, err := h.client.MessagesGetDialogFilters(ctx)
	if err != nil {
		return errResult(fmt.Sprintf("Failed to read folders: %v", err)), nil, nil
	}
	if !slices.Contains(folderIDs(filters), in.FolderID) {
		return errResult(fmt.Sprintf("No folder with ID %d. List your folders with GetFolders.", in.FolderID)), nil, nil
	}

	confirmed, err := confirmDestructive(ctx, req, fmt.Sprintf(
		"Delete folder %d? Your chats stay; only the folder view is removed.", in.FolderID,
	))
	if err != nil {
		return errResult(fmt.Sprintf("confirmation failed: %v", err)), nil, nil
	}
	if !confirmed {
		return textResult("Cancelled by user. The folder was not deleted."), nil, nil
	}

	// Omitting Filter (no SetFilter) tells Telegram to delete the folder.
	if _, err := h.client.MessagesUpdateDialogFilter(ctx, &tg.MessagesUpdateDialogFilterRequest{ID: in.FolderID}); err != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "DeleteFolder", map[string]any{"folder_id": in.FolderID, "error": err.Error()})
		return errResult(fmt.Sprintf("Failed to delete folder %d: %v", in.FolderID, err)), nil, nil
	}
	return nil, &DeleteFolderResult{Status: folderStatusDeleted, FolderID: in.FolderID}, nil
}

// ---------------------------------------------------------------------------
// AddChatsToFolder
// ---------------------------------------------------------------------------

// AddChatsToFolderHandler handles the AddChatsToFolder tool.
type AddChatsToFolderHandler struct {
	client *tg.Client
}

// NewAddChatsToFolderHandler creates a new AddChatsToFolderHandler.
func NewAddChatsToFolderHandler(client *tg.Client) *AddChatsToFolderHandler {
	return &AddChatsToFolderHandler{client: client}
}

// AddChatsToFolderInput is the input for the AddChatsToFolder tool.
type AddChatsToFolderInput struct {
	FolderID int      `json:"folder_id" jsonschema:"Numeric folder ID to add chats to (from GetFolders)"`
	Chats    []string `json:"chats" jsonschema:"One or more chats to add: each a public @username or a numeric chat ID"`
}

// AddChatsToFolderResult is the typed output of AddChatsToFolder.
type AddChatsToFolderResult struct {
	FolderID       int                 `json:"folder_id"`
	Added          []int64             `json:"added,omitempty"`
	AlreadyPresent []int64             `json:"already_present,omitempty"`
	Skipped        []FolderSkippedChat `json:"skipped,omitempty"`
}

// Register adds the AddChatsToFolder tool to the MCP server.
func (h *AddChatsToFolderHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "AddChatsToFolder",
		Description: "Add one or more chats, groups, or channels to an existing Telegram folder. Identify the folder by its numeric folder_id (from GetFolders) and pass chats as @usernames or numeric chat IDs. Chats already in the folder are reported as already_present; references that can't be resolved are reported as skipped. Reversible with RemoveChatsFromFolder.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *AddChatsToFolderHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in AddChatsToFolderInput) (*mcp.CallToolResult, *AddChatsToFolderResult, error) {
	if in.FolderID <= 0 {
		return errResult("folder_id is required: a positive folder ID from GetFolders."), nil, nil
	}
	if len(in.Chats) == 0 {
		return errResult("chats is required: one or more @usernames or numeric chat IDs to add."), nil, nil
	}

	filter, errRes := findEditableFolder(ctx, h.client, in.FolderID)
	if errRes != nil {
		return errRes, nil, nil
	}

	peers, skipped, ferr := resolveChatRefs(ctx, h.client, in.Chats)
	if ferr != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "AddChatsToFolder", map[string]any{"folder_id": in.FolderID, "error": ferr.Error()})
		return errResult(fmt.Sprintf("Aborted updating folder %d: %v. Retry once the condition clears.", in.FolderID, ferr)), nil, nil
	}

	out := &AddChatsToFolderResult{FolderID: in.FolderID, Skipped: skipped}
	out.Added, out.AlreadyPresent = applyAdditions(filter, peers)

	if len(out.Added) == 0 {
		// Nothing changed. Distinguish a benign no-op (everything already in the
		// folder) from a total failure (every reference skipped) so the latter
		// surfaces as an error instead of a successful-looking empty result.
		if len(out.AlreadyPresent) == 0 && len(out.Skipped) > 0 {
			return errResult(fmt.Sprintf("None of the chats could be added to folder %d: %s. Verify the references with ResolveUsername or SearchChats.", in.FolderID, formatSkipped(out.Skipped))), nil, nil
		}
		return nil, out, nil // skip the write
	}

	updReq := &tg.MessagesUpdateDialogFilterRequest{ID: in.FolderID}
	updReq.SetFilter(filter)
	if _, err := h.client.MessagesUpdateDialogFilter(ctx, updReq); err != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "AddChatsToFolder", map[string]any{"folder_id": in.FolderID, "error": err.Error()})
		return errResult(fmt.Sprintf("Failed to update folder %d: %v", in.FolderID, err)), nil, nil
	}
	return nil, out, nil
}

// ---------------------------------------------------------------------------
// RemoveChatsFromFolder
// ---------------------------------------------------------------------------

// RemoveChatsFromFolderHandler handles the RemoveChatsFromFolder tool.
type RemoveChatsFromFolderHandler struct {
	client *tg.Client
}

// NewRemoveChatsFromFolderHandler creates a new RemoveChatsFromFolderHandler.
func NewRemoveChatsFromFolderHandler(client *tg.Client) *RemoveChatsFromFolderHandler {
	return &RemoveChatsFromFolderHandler{client: client}
}

// RemoveChatsFromFolderInput is the input for the RemoveChatsFromFolder tool.
type RemoveChatsFromFolderInput struct {
	FolderID int      `json:"folder_id" jsonschema:"Numeric folder ID to remove chats from (from GetFolders)"`
	Chats    []string `json:"chats" jsonschema:"One or more chats to remove: each a public @username or a numeric chat ID"`
}

// RemoveChatsFromFolderResult is the typed output of RemoveChatsFromFolder.
type RemoveChatsFromFolderResult struct {
	FolderID   int                 `json:"folder_id"`
	Removed    []int64             `json:"removed,omitempty"`
	NotPresent []int64             `json:"not_present,omitempty"`
	Skipped    []FolderSkippedChat `json:"skipped,omitempty"`
}

// Register adds the RemoveChatsFromFolder tool to the MCP server.
func (h *RemoveChatsFromFolderHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "RemoveChatsFromFolder",
		Description: "Remove one or more chats, groups, or channels from an existing Telegram folder. Identify the folder by its numeric folder_id (from GetFolders) and pass chats as @usernames or numeric chat IDs. This only drops them from the folder's explicit include/pinned lists; the chats themselves are untouched. Chats not in the folder are reported as not_present. To remove the whole folder, use DeleteFolder.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *RemoveChatsFromFolderHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in RemoveChatsFromFolderInput) (*mcp.CallToolResult, *RemoveChatsFromFolderResult, error) {
	if in.FolderID <= 0 {
		return errResult("folder_id is required: a positive folder ID from GetFolders."), nil, nil
	}
	if len(in.Chats) == 0 {
		return errResult("chats is required: one or more @usernames or numeric chat IDs to remove."), nil, nil
	}

	filter, errRes := findEditableFolder(ctx, h.client, in.FolderID)
	if errRes != nil {
		return errRes, nil, nil
	}

	peers, skipped, ferr := resolveChatRefs(ctx, h.client, in.Chats)
	if ferr != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "RemoveChatsFromFolder", map[string]any{"folder_id": in.FolderID, "error": ferr.Error()})
		return errResult(fmt.Sprintf("Aborted updating folder %d: %v. Retry once the condition clears.", in.FolderID, ferr)), nil, nil
	}

	out := &RemoveChatsFromFolderResult{FolderID: in.FolderID, Skipped: skipped}
	out.Removed, out.NotPresent = applyRemovals(filter, peers)

	if len(out.Removed) == 0 {
		// Nothing changed: tell a total failure (all skipped) apart from a
		// benign no-op (none of the chats were in the folder).
		if len(out.NotPresent) == 0 && len(out.Skipped) > 0 {
			return errResult(fmt.Sprintf("None of the chats could be removed from folder %d: %s. Verify the references with ResolveUsername or SearchChats.", in.FolderID, formatSkipped(out.Skipped))), nil, nil
		}
		return nil, out, nil // skip the write
	}
	if !filterHasInclusion(filter) {
		return errResult(fmt.Sprintf("Removing those chats would leave folder %d empty, which Telegram doesn't allow. Use DeleteFolder to remove the folder instead.", in.FolderID)), nil, nil
	}

	updReq := &tg.MessagesUpdateDialogFilterRequest{ID: in.FolderID}
	updReq.SetFilter(filter)
	if _, err := h.client.MessagesUpdateDialogFilter(ctx, updReq); err != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "RemoveChatsFromFolder", map[string]any{"folder_id": in.FolderID, "error": err.Error()})
		return errResult(fmt.Sprintf("Failed to update folder %d: %v", in.FolderID, err)), nil, nil
	}
	return nil, out, nil
}
