package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// resolveFolderChats is the resolve step of tgclient.WithPeersFrom for a
// folder edit: it resolves refs — each a public @username or a numeric chat
// ID — to peers of any kind, since folders can hold users, channels and basic
// groups alike. Every returned peer's Input carries a bare ID (peerBareID).
//
// A problem with a reference itself (typo, unknown ID, no shared dialog,
// invite link, a form without a bare ID) is recorded in *skipped so the edit
// goes on with the other chats. Any other failure (rate limit, cancellation,
// dead session, a transport or server error) says nothing about the chat, so
// the edit fails as op and the model retries it instead of losing a good
// chat.
func resolveFolderChats(ctx context.Context, resolver *tgclient.Resolver, refs []string, op string, skipped *[]FolderSkippedChat) func() ([]tgclient.Peer, error) {
	return func() ([]tgclient.Peer, error) {
		*skipped = nil
		skip := func(ref, reason string) {
			*skipped = append(*skipped, FolderSkippedChat{Chat: ref, Reason: reason})
		}
		var peers []tgclient.Peer
		for _, ref := range refs {
			if strings.TrimSpace(ref) == "" {
				skip(ref, "empty chat reference")
				continue
			}
			peer, err := resolveChatRef(ctx, resolver, strings.TrimSpace(ref))
			switch {
			case errors.Is(err, errInviteChatRef):
				skip(ref, "is an invite link; join the chat first with JoinChat, then add it by @username or numeric ID")
			case err != nil && !tgclient.IsSystemic(err) && (tgclient.IsPeerSpecific(err) || errors.Is(err, errResolvedNotPresent)):
				skip(ref, err.Error())
			case err != nil:
				return nil, failed(op, err)
			default:
				if _, _, ok := peerBareID(peer.Input); !ok {
					skip(ref, "resolved to an unsupported peer form")
					continue
				}
				peers = append(peers, peer)
			}
		}
		return peers, nil
	}
}

// peerInputs returns the InputPeers of peers.
func peerInputs(peers []tgclient.Peer) []tg.InputPeerClass {
	inputs := make([]tg.InputPeerClass, len(peers))
	for i, peer := range peers {
		inputs[i] = peer.Input
	}
	return inputs
}

// applyAdditions adds each peer to the folder's include list, de-duplicating
// against both the include and pinned lists (a pinned chat is already in the
// folder), and drops each added peer from the exclude list so it isn't both
// included and excluded. It returns the bare IDs actually added and those
// already present. Every peer must carry a bare ID (guaranteed by
// resolveFolderChats).
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

// warnSkipped warns on p of the chat references a folder edit skipped.
func warnSkipped(p *partialOutcome, skipped []FolderSkippedChat) {
	if len(skipped) > 0 {
		p.warn(fmt.Sprintf("%d of the chats were skipped; skipped says why.", len(skipped)))
	}
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

// findEditableFolder fetches all folders and returns the standard DialogFilter
// with the given ID, ready for an in-place edit. It fails when the ID is
// unknown or names a shared (imported chatlist) folder, whose chats cannot be
// edited through updateDialogFilter. op words the failure.
func findEditableFolder(ctx context.Context, client *tg.Client, folderID int, op string) (*tg.DialogFilter, error) {
	filters, err := client.MessagesGetDialogFilters(ctx)
	if err != nil {
		return nil, failed(op, fmt.Errorf("reading folders: %w", err))
	}
	for _, f := range filters.Filters {
		switch df := f.(type) {
		case *tg.DialogFilter:
			if df.ID == folderID {
				return df, nil
			}
		case *tg.DialogFilterChatlist:
			if df.ID == folderID {
				return nil, failedHint(op, errors.New("it is a shared/imported folder, whose chats can't be edited here"), "Manage it from the Telegram app instead.")
			}
		}
	}
	return nil, failedHint(op, fmt.Errorf("no folder with ID %d", folderID), "List your folders with GetFolders to find the right ID.")
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
	AddTool(s, &mcp.Tool{
		Name:        "GetFolders",
		Description: "List your Telegram chat folders (dialog filters) with their numeric ID, title, category flags, and the bare IDs of included/excluded/pinned chats. Call this first to find the folder_id needed by AddChatsToFolder, RemoveChatsFromFolder, and DeleteFolder. Shared/imported folders are reported with kind \"shared_folder\" and cannot have their chats edited here.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *GetFoldersHandler) handle(ctx context.Context, _ *mcp.CallToolRequest, _ GetFoldersInput) (*mcp.CallToolResult, *GetFoldersResult, error) {
	filters, err := h.client.MessagesGetDialogFilters(ctx)
	if err != nil {
		return nil, nil, failed("get folders", err)
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
	peers *tgclient.Resolver
}

// NewCreateFolderHandler creates a new CreateFolderHandler.
func NewCreateFolderHandler(peers *tgclient.Resolver) *CreateFolderHandler {
	return &CreateFolderHandler{peers: peers}
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
	partialOutcome
}

// hasCategoryInclude reports whether any include_* category flag is set.
func (in CreateFolderInput) hasCategoryInclude() bool {
	return in.IncludeContacts || in.IncludeNonContacts || in.IncludeGroups ||
		in.IncludeChannels || in.IncludeBots
}

// Register adds the CreateFolder tool to the MCP server.
func (h *CreateFolderHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name:        "CreateFolder",
		Description: "Create a new Telegram chat folder (dialog filter). Provide a title (max 12 characters) and at least one source of chats: an explicit list of @usernames / numeric chat IDs, and/or category flags like include_groups or include_channels. The folder ID is assigned automatically. Add or remove chats later with AddChatsToFolder / RemoveChatsFromFolder.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *CreateFolderHandler) handle(ctx context.Context, _ *mcp.CallToolRequest, in CreateFolderInput) (*mcp.CallToolResult, *CreateFolderResult, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return ErrResult("title is required: a short folder name (max 12 characters)."), nil, nil
	}
	if utf8.RuneCountInString(title) > maxFolderTitleRunes {
		return ErrResult(fmt.Sprintf("title %q is too long: max %d characters.", title, maxFolderTitleRunes)), nil, nil
	}
	if len(in.Chats) == 0 && !in.hasCategoryInclude() {
		return ErrResult("a folder needs at least one chat to include or one include_* category flag (e.g. include_groups). Telegram rejects empty folders."), nil, nil
	}

	op := fmt.Sprintf("create folder %q", title)
	filters, err := h.peers.Client().MessagesGetDialogFilters(ctx)
	if err != nil {
		return nil, nil, failed(op, fmt.Errorf("reading existing folders: %w", err))
	}
	id := nextFolderID(folderIDs(filters))

	var skipped []FolderSkippedChat
	result, err := tgclient.WithPeersFrom(h.peers, resolveFolderChats(ctx, h.peers, in.Chats, op, &skipped), func(peers []tgclient.Peer) (*CreateFolderResult, error) {
		return h.create(ctx, in, op, id, title, peerInputs(peers), skipped)
	})
	if err != nil {
		return nil, nil, err
	}
	return nil, result, nil
}

// create writes folder id holding peers and the input's category flags.
func (h *CreateFolderHandler) create(ctx context.Context, in CreateFolderInput, op string, id int, title string, peers []tg.InputPeerClass, skipped []FolderSkippedChat) (*CreateFolderResult, error) {
	includePeers := make([]tg.InputPeerClass, 0, len(peers))
	for _, peer := range peers {
		if !containsPeer(includePeers, peer) {
			includePeers = append(includePeers, peer)
		}
	}
	if len(includePeers) == 0 && !in.hasCategoryInclude() {
		return nil, failedHint(op, fmt.Errorf("none of the provided chats could be resolved (%s) and no include_* flag was set, so the folder would be empty", formatSkipped(skipped)), "Check the chat references (try ResolveUsername or SearchChats) and retry.")
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
	if _, err := h.peers.Client().MessagesUpdateDialogFilter(ctx, updReq); err != nil {
		if tgerr.Is(err, "DIALOG_FILTERS_TOO_MUCH") {
			return nil, failedHint(op, err, "You've reached the maximum number of folders. Delete one with DeleteFolder first, or a Telegram Premium subscription raises the limit.")
		}
		return nil, failed(op, err)
	}
	out := &CreateFolderResult{
		FolderID:      id,
		Title:         title,
		IncludedCount: len(includePeers),
		Skipped:       skipped,
	}
	warnSkipped(&out.partialOutcome, skipped)
	return out, nil
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
	FolderID int  `json:"folder_id" jsonschema:"Numeric folder ID to delete (from GetFolders)"`
	Confirm  bool `json:"confirm" jsonschema:"Must be true after the user explicitly confirms deleting the folder"`
}

// DeleteFolderResult is the typed output of DeleteFolder.
type DeleteFolderResult struct {
	Status   string `json:"status"` // "deleted"
	FolderID int    `json:"folder_id"`
}

// Register adds the DeleteFolder tool to the MCP server.
func (h *DeleteFolderHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name:        "DeleteFolder",
		Description: "Delete a Telegram chat folder (dialog filter) by its numeric ID. This removes only the folder view — your chats and their messages are untouched. Find the folder_id with GetFolders. The call is rejected unless confirm=true.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: new(true), OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *DeleteFolderHandler) handle(ctx context.Context, _ *mcp.CallToolRequest, in DeleteFolderInput) (*mcp.CallToolResult, *DeleteFolderResult, error) {
	if in.FolderID <= 0 {
		return ErrResult("folder_id is required: a positive folder ID from GetFolders."), nil, nil
	}
	if errRes := requireExplicitConfirmation(in.Confirm, "delete the folder"); errRes != nil {
		return errRes, nil, nil
	}

	op := fmt.Sprintf("delete folder %d", in.FolderID)
	filters, err := h.client.MessagesGetDialogFilters(ctx)
	if err != nil {
		return nil, nil, failed(op, fmt.Errorf("reading folders: %w", err))
	}
	if !slices.Contains(folderIDs(filters), in.FolderID) {
		return nil, nil, failedHint(op, errors.New("no folder with that ID"), "List your folders with GetFolders.")
	}

	// Omitting Filter (no SetFilter) tells Telegram to delete the folder.
	if _, err := h.client.MessagesUpdateDialogFilter(ctx, &tg.MessagesUpdateDialogFilterRequest{ID: in.FolderID}); err != nil {
		return nil, nil, failed(op, err)
	}
	return nil, &DeleteFolderResult{Status: folderStatusDeleted, FolderID: in.FolderID}, nil
}

// ---------------------------------------------------------------------------
// AddChatsToFolder
// ---------------------------------------------------------------------------

// AddChatsToFolderHandler handles the AddChatsToFolder tool.
type AddChatsToFolderHandler struct {
	peers *tgclient.Resolver
}

// NewAddChatsToFolderHandler creates a new AddChatsToFolderHandler.
func NewAddChatsToFolderHandler(peers *tgclient.Resolver) *AddChatsToFolderHandler {
	return &AddChatsToFolderHandler{peers: peers}
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
	partialOutcome
}

// Register adds the AddChatsToFolder tool to the MCP server.
func (h *AddChatsToFolderHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name:        "AddChatsToFolder",
		Description: "Add one or more chats, groups, or channels to an existing Telegram folder. Identify the folder by its numeric folder_id (from GetFolders) and pass chats as @usernames or numeric chat IDs. Chats already in the folder are reported as already_present; references that can't be resolved are reported as skipped. Reversible with RemoveChatsFromFolder.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *AddChatsToFolderHandler) handle(ctx context.Context, _ *mcp.CallToolRequest, in AddChatsToFolderInput) (*mcp.CallToolResult, *AddChatsToFolderResult, error) {
	if in.FolderID <= 0 {
		return ErrResult("folder_id is required: a positive folder ID from GetFolders."), nil, nil
	}
	if len(in.Chats) == 0 {
		return ErrResult("chats is required: one or more @usernames or numeric chat IDs to add."), nil, nil
	}

	added, present, skipped, err := editFolderChats(ctx, h.peers, in.FolderID, in.Chats, "added to", applyAdditions)
	if err != nil {
		return nil, nil, err
	}
	out := &AddChatsToFolderResult{FolderID: in.FolderID, Added: added, AlreadyPresent: present, Skipped: skipped}
	warnSkipped(&out.partialOutcome, skipped)
	return nil, out, nil
}

// ---------------------------------------------------------------------------
// RemoveChatsFromFolder
// ---------------------------------------------------------------------------

// RemoveChatsFromFolderHandler handles the RemoveChatsFromFolder tool.
type RemoveChatsFromFolderHandler struct {
	peers *tgclient.Resolver
}

// NewRemoveChatsFromFolderHandler creates a new RemoveChatsFromFolderHandler.
func NewRemoveChatsFromFolderHandler(peers *tgclient.Resolver) *RemoveChatsFromFolderHandler {
	return &RemoveChatsFromFolderHandler{peers: peers}
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
	partialOutcome
}

// Register adds the RemoveChatsFromFolder tool to the MCP server.
func (h *RemoveChatsFromFolderHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name:        "RemoveChatsFromFolder",
		Description: "Remove one or more chats, groups, or channels from an existing Telegram folder. Identify the folder by its numeric folder_id (from GetFolders) and pass chats as @usernames or numeric chat IDs. This only drops them from the folder's explicit include/pinned lists; the chats themselves are untouched. Chats not in the folder are reported as not_present. To remove the whole folder, use DeleteFolder.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *RemoveChatsFromFolderHandler) handle(ctx context.Context, _ *mcp.CallToolRequest, in RemoveChatsFromFolderInput) (*mcp.CallToolResult, *RemoveChatsFromFolderResult, error) {
	if in.FolderID <= 0 {
		return ErrResult("folder_id is required: a positive folder ID from GetFolders."), nil, nil
	}
	if len(in.Chats) == 0 {
		return ErrResult("chats is required: one or more @usernames or numeric chat IDs to remove."), nil, nil
	}

	removed, absent, skipped, err := editFolderChats(ctx, h.peers, in.FolderID, in.Chats, "removed from", applyRemovals)
	if err != nil {
		return nil, nil, err
	}
	out := &RemoveChatsFromFolderResult{FolderID: in.FolderID, Removed: removed, NotPresent: absent, Skipped: skipped}
	warnSkipped(&out.partialOutcome, skipped)
	return nil, out, nil
}

// editFolderChats resolves chats, applies them to folder folderID with apply,
// and writes the folder back when that changed anything. changed and unchanged
// are apply's split of the resolved chats; verb ("added to", "removed from")
// words the error for a call where every reference was skipped.
func editFolderChats(
	ctx context.Context,
	peers *tgclient.Resolver,
	folderID int,
	chats []string,
	verb string,
	apply func(*tg.DialogFilter, []tg.InputPeerClass) (changed, unchanged []int64),
) (changed, unchanged []int64, skipped []FolderSkippedChat, err error) {
	op := fmt.Sprintf("update folder %d", folderID)
	stored, err := findEditableFolder(ctx, peers.Client(), folderID, op)
	if err != nil {
		return nil, nil, nil, err
	}
	type edit struct{ changed, unchanged []int64 }
	done, err := tgclient.WithPeersFrom(peers, resolveFolderChats(ctx, peers, chats, op, &skipped), func(resolved []tgclient.Peer) (edit, error) {
		// Each attempt edits its own copy, so a retry starts from the folder
		// as Telegram holds it.
		filter := *stored
		filter.PinnedPeers = slices.Clone(stored.PinnedPeers)
		filter.IncludePeers = slices.Clone(stored.IncludePeers)
		filter.ExcludePeers = slices.Clone(stored.ExcludePeers)
		changed, unchanged := apply(&filter, peerInputs(resolved))
		if len(changed) == 0 {
			// Nothing changed. Distinguish a benign no-op (every chat already in
			// the wanted state) from a total failure (every reference skipped) so
			// the latter surfaces as an error instead of a successful-looking
			// empty result.
			if len(unchanged) == 0 && len(skipped) > 0 {
				return edit{}, failedHint(op, fmt.Errorf("none of the chats could be %s it: %s", verb, formatSkipped(skipped)), "Verify the references with ResolveUsername or SearchChats.")
			}
			return edit{changed, unchanged}, nil // skip the write
		}
		// Only a removal can leave the folder without an inclusion.
		if !filterHasInclusion(&filter) {
			return edit{}, failedHint(op, errors.New("removing those chats would leave the folder empty, which Telegram doesn't allow"), "Use DeleteFolder to remove the folder instead.")
		}
		updReq := &tg.MessagesUpdateDialogFilterRequest{ID: folderID}
		updReq.SetFilter(&filter)
		if _, err := peers.Client().MessagesUpdateDialogFilter(ctx, updReq); err != nil {
			return edit{}, failed(op, err)
		}
		return edit{changed, unchanged}, nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return done.changed, done.unchanged, skipped, nil
}
