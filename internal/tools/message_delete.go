package tools

import (
	"context"
	"fmt"
	"slices"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/presentation"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// maxDeleteBatch is Telegram's per-call limit for messages.deleteMessages,
// channels.deleteMessages and the matching getMessages calls.
const maxDeleteBatch = 100

// Per-message outcomes of DeleteMessages.
const (
	statusDeleted   = "deleted"
	statusNotFound  = "not_found"
	statusForbidden = "forbidden"
)

// statusCompleted is the top-level DeleteMessages status once the batch has
// been processed; the per-message outcomes are in Results.
const statusCompleted = "completed"

// deleteRightsHint explains a forbidden outcome.
const deleteRightsHint = "Deleting other members' messages needs admin rights with the delete-messages permission in this chat."

// MessageDeleteHandler handles the DeleteMessages tool.
type MessageDeleteHandler struct {
	client *tg.Client
}

// NewMessageDeleteHandler creates a new MessageDeleteHandler.
func NewMessageDeleteHandler(client *tg.Client) *MessageDeleteHandler {
	return &MessageDeleteHandler{client: client}
}

// DeleteMessagesInput is the input for the DeleteMessages tool.
//
// MessageIDs are opaque handles returned by GetMessages or SendMessage:
//   - "42"   → delete a regular (delivered) message
//   - "s:42" → cancel a pending scheduled message
//
// A batch holds one kind only, so a failed Telegram call never leaves the
// other kind half-processed.
type DeleteMessagesInput struct {
	ChatID     int64    `json:"chat_id" jsonschema:"The ID of the chat containing the messages"`
	MessageIDs []string `json:"message_ids" jsonschema:"1-100 opaque message handles from GetMessages or SendMessage. All regular (\"42\") or all scheduled (\"s:42\")\\, not mixed."`
	Confirm    bool     `json:"confirm" jsonschema:"Must be true after the user explicitly confirms this irreversible deletion"`
}

// DeletedMessage is the outcome for one requested handle.
type DeletedMessage struct {
	MessageID string `json:"message_id"`
	Status    string `json:"status"` // "deleted" | "not_found" | "forbidden"
}

// DeleteMessagesResult is the typed output of DeleteMessages. "deleted" is
// verified by re-reading the messages after the call, never assumed from the
// call succeeding.
type DeleteMessagesResult struct {
	Status  string           `json:"status"` // "completed"
	ChatID  int64            `json:"chat_id"`
	Deleted int              `json:"deleted"`
	Results []DeletedMessage `json:"results,omitempty"`
	Hint    string           `json:"hint,omitempty"`
}

// Register adds the tool to the MCP server.
func (h *MessageDeleteHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name:        "DeleteMessages",
		Description: "Delete up to 100 messages from one chat in a single call. Regular handles (\"42\") delete delivered messages for all participants and cannot be undone; scheduled handles (\"s:42\") cancel pending delivery. One kind per call. Reports each message as deleted (verified gone), not_found (no such message in this chat) or forbidden (you lack the right to delete it for everyone — left untouched rather than deleted only for you). The call is rejected unless confirm=true. Use GetMessages first (with include_scheduled=true for the scheduled queue) to get the handles.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: new(true), OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *MessageDeleteHandler) handle(ctx context.Context, _ *mcp.CallToolRequest, in DeleteMessagesInput) (*mcp.CallToolResult, *DeleteMessagesResult, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}
	if len(in.MessageIDs) == 0 || len(in.MessageIDs) > maxDeleteBatch {
		return errResult(fmt.Sprintf("message_ids must hold 1-%d handles, got %d. Split larger cleanups into several calls.", maxDeleteBatch, len(in.MessageIDs))), nil, nil
	}

	var ids []int
	scheduled := false
	for i, s := range in.MessageIDs {
		ref, err := presentation.ParseMessageRef(s)
		if err != nil {
			return errInvalidMessageID("message_ids", s, err), nil, nil
		}
		if i == 0 {
			scheduled = ref.Scheduled
		} else if ref.Scheduled != scheduled {
			return errResult("message_ids mixes regular (\"42\") and scheduled (\"s:42\") handles. Delete each kind in its own call."), nil, nil
		}
		if !slices.Contains(ids, ref.ID) {
			ids = append(ids, ref.ID)
		}
	}
	if errRes := requireExplicitConfirmation(in.Confirm, "delete the messages"); errRes != nil {
		return errRes, nil, nil
	}

	peer, err := tgclient.ResolvePeer(ctx, h.client, in.ChatID)
	if err != nil {
		return nil, nil, failed(fmt.Sprintf("delete messages in chat %d", in.ChatID), err)
	}

	var statuses map[int]string
	if scheduled {
		statuses, err = h.deleteScheduled(ctx, peer, ids)
	} else {
		statuses, err = h.deleteRegular(ctx, peer, ids)
	}
	if err != nil {
		return nil, nil, err
	}

	res := &DeleteMessagesResult{Status: statusCompleted, ChatID: in.ChatID}
	for _, id := range ids {
		handle := presentation.FormatRegularRef(id)
		if scheduled {
			handle = presentation.MessageRef{ID: id, Scheduled: true}.Format()
		}
		st := statuses[id]
		switch st {
		case statusDeleted:
			res.Deleted++
		case statusForbidden:
			res.Hint = deleteRightsHint + " Messages marked forbidden were left untouched."
		}
		res.Results = append(res.Results, DeletedMessage{MessageID: handle, Status: st})
	}
	return nil, res, nil
}

// deleteRegular deletes delivered messages. messages.deleteMessages takes no
// peer (IDs are global across the account's non-channel chats), so each ID is
// first read back and kept only if it belongs to this chat — a stray ID must
// never delete a message elsewhere. A message you may not delete for everyone
// is reported forbidden and not sent: with revoke Telegram would still delete
// it for you alone, hiding it from you while others keep seeing it.
func (h *MessageDeleteHandler) deleteRegular(ctx context.Context, peer tg.InputPeerClass, ids []int) (map[int]string, error) {
	msgs, chats, err := h.getRegular(ctx, peer, ids)
	if err != nil {
		return nil, failed("read the messages to delete", err)
	}
	canDeleteOthers := canDeleteOthersMessages(peer, chats)

	statuses := make(map[int]string, len(ids))
	var toDelete []int
	for _, id := range ids {
		m, ok := msgs[id]
		switch {
		case !ok:
			statuses[id] = statusNotFound
		case !m.GetOut() && !canDeleteOthers:
			statuses[id] = statusForbidden
		default:
			toDelete = append(toDelete, id)
		}
	}
	if len(toDelete) == 0 {
		return statuses, nil
	}

	if ch, ok := peer.(*tg.InputPeerChannel); ok {
		_, err = h.client.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
			ID:      toDelete,
		})
	} else {
		_, err = h.client.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{Revoke: true, ID: toDelete})
	}
	if err != nil {
		return nil, deleteFailure(err)
	}

	left, _, err := h.getRegular(ctx, peer, toDelete)
	if err != nil {
		return nil, failedHint("re-read the messages after Telegram accepted the deletion", err, "Check with GetMessages which of them are gone.")
	}
	markVerified(statuses, toDelete, left)
	return statuses, nil
}

// deleteScheduled cancels pending scheduled messages. The schedule queue is
// per chat and holds only your own messages, so there is no ownership check.
func (h *MessageDeleteHandler) deleteScheduled(ctx context.Context, peer tg.InputPeerClass, ids []int) (map[int]string, error) {
	msgs, err := h.getScheduled(ctx, peer, ids)
	if err != nil {
		return nil, failed("read the scheduled messages to delete", err)
	}
	statuses := make(map[int]string, len(ids))
	var toDelete []int
	for _, id := range ids {
		if _, ok := msgs[id]; ok {
			toDelete = append(toDelete, id)
		} else {
			statuses[id] = statusNotFound
		}
	}
	if len(toDelete) == 0 {
		return statuses, nil
	}

	if _, err := h.client.MessagesDeleteScheduledMessages(ctx, &tg.MessagesDeleteScheduledMessagesRequest{Peer: peer, ID: toDelete}); err != nil {
		return nil, deleteFailure(err)
	}

	left, err := h.getScheduled(ctx, peer, toDelete)
	if err != nil {
		return nil, failedHint("re-read the schedule queue after Telegram accepted the cancellation", err, "Check with GetMessages include_scheduled=true which of them are gone.")
	}
	markVerified(statuses, toDelete, left)
	return statuses, nil
}

// markVerified sets deleted/forbidden for each attempted ID from what is
// still readable after the delete call.
func markVerified(statuses map[int]string, attempted []int, left map[int]tg.NotEmptyMessage) {
	for _, id := range attempted {
		if _, still := left[id]; still {
			statuses[id] = statusForbidden
		} else {
			statuses[id] = statusDeleted
		}
	}
}

// getRegular reads delivered messages by ID and returns those that exist in
// this chat, plus the chats the response carried (for the rights check).
func (h *MessageDeleteHandler) getRegular(ctx context.Context, peer tg.InputPeerClass, ids []int) (map[int]tg.NotEmptyMessage, []tg.ChatClass, error) {
	inputIDs := make([]tg.InputMessageClass, len(ids))
	for i, id := range ids {
		inputIDs[i] = &tg.InputMessageID{ID: id}
	}
	var resp tg.MessagesMessagesClass
	var err error
	if ch, ok := peer.(*tg.InputPeerChannel); ok {
		resp, err = h.client.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
			ID:      inputIDs,
		})
	} else {
		resp, err = h.client.MessagesGetMessages(ctx, inputIDs)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get messages: %w", err)
	}
	modified, ok := resp.AsModified()
	if !ok {
		return nil, nil, fmt.Errorf("unexpected %T response", resp)
	}
	return messagesInPeer(modified.GetMessages(), peer), modified.GetChats(), nil
}

// getScheduled reads scheduled messages by ID from this chat's queue.
func (h *MessageDeleteHandler) getScheduled(ctx context.Context, peer tg.InputPeerClass, ids []int) (map[int]tg.NotEmptyMessage, error) {
	resp, err := h.client.MessagesGetScheduledMessages(ctx, &tg.MessagesGetScheduledMessagesRequest{Peer: peer, ID: ids})
	if err != nil {
		return nil, fmt.Errorf("get scheduled messages: %w", err)
	}
	modified, ok := resp.AsModified()
	if !ok {
		return nil, fmt.Errorf("unexpected %T response", resp)
	}
	return messagesInPeer(modified.GetMessages(), peer), nil
}

// messagesInPeer indexes the non-empty messages that belong to peer by ID.
func messagesInPeer(msgs []tg.MessageClass, peer tg.InputPeerClass) map[int]tg.NotEmptyMessage {
	out := make(map[int]tg.NotEmptyMessage, len(msgs))
	for _, mc := range msgs {
		m, ok := mc.AsNotEmpty()
		if ok && peerIs(m.GetPeerID(), peer) {
			out[m.GetID()] = m
		}
	}
	return out
}

// peerIs reports whether a message's peer is the given input peer.
func peerIs(p tg.PeerClass, in tg.InputPeerClass) bool {
	switch v := in.(type) {
	case *tg.InputPeerUser:
		u, ok := p.(*tg.PeerUser)
		return ok && u.UserID == v.UserID
	case *tg.InputPeerChat:
		c, ok := p.(*tg.PeerChat)
		return ok && c.ChatID == v.ChatID
	case *tg.InputPeerChannel:
		c, ok := p.(*tg.PeerChannel)
		return ok && c.ChannelID == v.ChannelID
	default:
		return false
	}
}

// canDeleteOthersMessages reports whether this account may delete other
// members' messages for everyone: always in a private chat, otherwise only as
// creator or as an admin with the delete-messages right.
func canDeleteOthersMessages(peer tg.InputPeerClass, chats []tg.ChatClass) bool {
	switch v := peer.(type) {
	case *tg.InputPeerUser:
		return true
	case *tg.InputPeerChat:
		for _, c := range chats {
			if chat, ok := c.(*tg.Chat); ok && chat.ID == v.ChatID {
				rights, hasRights := chat.GetAdminRights()
				return chat.GetCreator() || (hasRights && rights.DeleteMessages)
			}
		}
	case *tg.InputPeerChannel:
		for _, c := range chats {
			if ch, ok := c.(*tg.Channel); ok && ch.ID == v.ChannelID {
				rights, hasRights := ch.GetAdminRights()
				return ch.GetCreator() || (hasRights && rights.DeleteMessages)
			}
		}
	}
	return false
}

// deleteFailure reports a failed delete call with Telegram's error as-is,
// adding the rights hint when Telegram refused on permission grounds.
func deleteFailure(err error) error {
	if tgerr.Is(err, "MESSAGE_DELETE_FORBIDDEN", "CHAT_ADMIN_REQUIRED") {
		return failedHint("delete messages", err, deleteRightsHint)
	}
	return failed("delete messages", err)
}
