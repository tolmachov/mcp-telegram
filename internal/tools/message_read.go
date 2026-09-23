package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// maxMarkAsReadChats caps the chats one MarkAsRead call takes, which also
// keeps their channels' top-message lookup to one messages.getPeerDialogs
// request.
const maxMarkAsReadChats = 100

// MessageReadHandler handles the MarkAsRead tool.
type MessageReadHandler struct {
	client *tg.Client
	peers  *tgclient.Resolver
}

// NewMessageReadHandler creates a new MessageReadHandler.
func NewMessageReadHandler(peers *tgclient.Resolver) *MessageReadHandler {
	return &MessageReadHandler{client: peers.Client(), peers: peers}
}

// MarkAsReadInput is the input for the MarkAsRead tool.
type MarkAsReadInput struct {
	ChatIDs []int64 `json:"chat_ids" jsonschema:"List of chat IDs to mark as read (max 100)"`
}

// MarkAsReadFailure describes a single chat that failed to be marked as read.
type MarkAsReadFailure struct {
	ChatID int64  `json:"chat_id"`
	Error  string `json:"error"`
}

// MarkAsReadResult is the typed output of MarkAsRead.
type MarkAsReadResult struct {
	Successful int                 `json:"successful"`
	Failed     int                 `json:"failed"`
	SuccessIDs []int64             `json:"success_ids,omitempty"`
	Failures   []MarkAsReadFailure `json:"failures,omitempty"`
	// TotalChats counts chats *attempted* (Successful + Failed), not chats
	// requested. When the batch stops early it ends before every input is
	// tried, so the requested total is TotalChats + len(SkippedIDs).
	TotalChats int `json:"total_chats"`
	// SkippedIDs holds chats not attempted because the batch stopped early on
	// a systemic failure (a flood wait, a dead session, a cancelled call);
	// Warning explains why. Both are empty on a normal run.
	SkippedIDs []int64 `json:"skipped_ids,omitempty"`
	Warning    string  `json:"warning,omitempty"`
}

// markReadResult is the internal per-chat outcome before formatting.
type markReadResult struct {
	chatID  int64
	success bool
	err     error
}

// Register adds the tool to the MCP server.
func (h *MessageReadHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name:        "MarkAsRead",
		Description: "Mark all messages in one or more chats as read (clears the unread badge). Accepts up to 100 chat IDs at once. Use GetChats to discover chat IDs. Idempotent — calling it again has the same effect as calling it once.",
		Annotations: &mcp.ToolAnnotations{
			IdempotentHint: true,
			OpenWorldHint:  new(true),
		},
	}, h.handle)
}

func (h *MessageReadHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in MarkAsReadInput) (*mcp.CallToolResult, *MarkAsReadResult, error) {
	if len(in.ChatIDs) == 0 {
		return errResult("chat_ids is required and must not be empty. Use GetChats to discover chat IDs first."), nil, nil
	}
	if len(in.ChatIDs) > maxMarkAsReadChats {
		return errResult(fmt.Sprintf("Cannot process more than %d chats at once. Split the request into batches.", maxMarkAsReadChats)), nil, nil
	}

	results := make([]markReadResult, 0, len(in.ChatIDs))
	// record adds one chat's outcome. Ordinary errors don't stop the batch —
	// partial failures are surfaced via the result payload AND via a
	// structured Warning log so MCP clients with a log panel can flag them. A
	// systemic error is the exception (tgclient.IsSystemic): a flood wait is
	// account-level and cumulative, so going on would deepen the limit, and a
	// dead session or a cancelled call fails every chat alike. record then
	// reports stop=true.
	record := func(chatID int64, err error) (stop bool) {
		results = append(results, markReadResult{chatID: chatID, success: err == nil, err: err})
		if err == nil {
			return false
		}
		mcpLog(ctx, req.Session, logLevelWarning, "MarkAsRead", map[string]any{
			"chat_id": chatID,
			"error":   err.Error(),
		})
		return tgclient.IsSystemic(err)
	}
	// stopped reports a batch cut short by the systemic err, with the chats
	// still pending as skipped so the model waits instead of retry-spamming.
	stopped := func(pending []int64, err error) (*mcp.CallToolResult, *MarkAsReadResult, error) {
		out := h.buildResult(results)
		out.SkippedIDs = append([]int64(nil), pending...)
		out.Warning = systemicText("MarkAsRead", err)
		return nil, out, nil
	}

	// Resolve every chat first: channels need their top message ID, which one
	// messages.getPeerDialogs call fetches for all of them.
	var pending, channelIDs []int64
	for i, chatID := range in.ChatIDs {
		peer, err := h.peers.Resolve(ctx, chatID)
		if err != nil {
			if record(chatID, err) {
				return stopped(append(pending, in.ChatIDs[i+1:]...), err)
			}
			continue
		}
		pending = append(pending, chatID)
		if _, ok := peer.Input.(*tg.InputPeerChannel); ok {
			channelIDs = append(channelIDs, chatID)
		}
	}

	// Telegram fails the shared lookup as a whole for one bad channel, so
	// unless the failure is systemic each channel then looks up its own top
	// message and only the bad one fails.
	tops, err := h.topMessages(ctx, channelIDs)
	if err != nil {
		if tgclient.IsSystemic(err) {
			return stopped(pending, err)
		}
		tops = nil
	}

	for i, chatID := range pending {
		if err := h.markChatAsRead(ctx, chatID, slices.Contains(channelIDs, chatID), tops); record(chatID, err) {
			return stopped(pending[i+1:], err)
		}
	}
	return h.finalResult(results)
}

// finalResult assembles the tool result for a completed (un-interrupted) batch,
// collapsing to an error when every chat failed.
func (h *MessageReadHandler) finalResult(results []markReadResult) (*mcp.CallToolResult, *MarkAsReadResult, error) {
	out := h.buildResult(results)
	if out.Failed > 0 && out.Successful == 0 {
		errs := make([]error, 0, len(results))
		for _, r := range results {
			errs = append(errs, fmt.Errorf("chat_id=%d: %w", r.chatID, r.err))
		}
		return nil, nil, failed(fmt.Sprintf("mark any of the %d chat(s) as read", out.Failed), errors.Join(errs...))
	}
	return nil, out, nil
}

// buildResult tallies per-chat outcomes into the structured result.
func (h *MessageReadHandler) buildResult(results []markReadResult) *MarkAsReadResult {
	out := &MarkAsReadResult{TotalChats: len(results)}
	for _, r := range results {
		if r.success {
			out.Successful++
			out.SuccessIDs = append(out.SuccessIDs, r.chatID)
		} else {
			out.Failed++
			out.Failures = append(out.Failures, MarkAsReadFailure{
				ChatID: r.chatID,
				Error:  fmt.Sprintf("%v", r.err),
			})
		}
	}
	return out
}

// topMessages returns the top message ID of each channel in channelIDs from
// one messages.getPeerDialogs call (maxMarkAsReadChats keeps the batch within
// a single request). A channel missing from the answer — one this account has
// no dialog with — gets none: it has no unread badge to clear. The map is
// non-nil on success.
func (h *MessageReadHandler) topMessages(ctx context.Context, channelIDs []int64) (map[int64]int, error) {
	if len(channelIDs) == 0 {
		return map[int64]int{}, nil
	}
	return tgclient.WithPeers(ctx, h.peers, channelIDs, nil, nil, func(peers []tgclient.Peer) (map[int64]int, error) {
		dialogPeers := make([]tg.InputDialogPeerClass, len(peers))
		for i, p := range peers {
			dialogPeers[i] = &tg.InputDialogPeer{Peer: p.Input}
		}
		res, err := h.client.MessagesGetPeerDialogs(ctx, dialogPeers)
		if err != nil {
			return nil, fmt.Errorf("getting channel top messages: %w", err)
		}
		tops := make(map[int64]int, len(res.Dialogs))
		for _, d := range res.Dialogs {
			dialog, ok := d.(*tg.Dialog)
			if !ok {
				continue
			}
			if ch, ok := dialog.Peer.(*tg.PeerChannel); ok {
				tops[ch.ChannelID] = dialog.TopMessage
			}
		}
		return tops, nil
	})
}

// markChatAsRead marks a single chat as read. tops holds the channels' top
// message IDs from the shared lookup, or is nil when that lookup failed, in
// which case a channel looks up its own.
func (h *MessageReadHandler) markChatAsRead(ctx context.Context, chatID int64, isChannel bool, tops map[int64]int) error {
	if isChannel && tops == nil {
		own, err := h.topMessages(ctx, []int64{chatID})
		if err != nil {
			return fmt.Errorf("marking chat as read: %w", err)
		}
		tops = own
	}
	_, err := tgclient.WithPeer(ctx, h.peers, chatID, nil, nil, func(p tgclient.Peer) (bool, error) {
		channel, ok := p.Input.(*tg.InputPeerChannel)
		if !ok {
			if _, err := h.client.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{Peer: p.Input}); err != nil {
				return false, fmt.Errorf("reading history: %w", err)
			}
			return true, nil
		}
		top := tops[chatID]
		if top == 0 {
			// Nothing to acknowledge, and Telegram rejects channels.readHistory
			// with a zero MaxID.
			return true, nil
		}
		return h.client.ChannelsReadHistory(ctx, &tg.ChannelsReadHistoryRequest{
			Channel: &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
			MaxID:   top,
		})
	})
	if err != nil {
		return fmt.Errorf("marking chat as read: %w", err)
	}
	return nil
}
