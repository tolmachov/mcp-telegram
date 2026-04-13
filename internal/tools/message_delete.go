package tools

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessageDeleteHandler handles the DeleteMessage tool.
type MessageDeleteHandler struct {
	client *tg.Client
}

// NewMessageDeleteHandler creates a new MessageDeleteHandler.
func NewMessageDeleteHandler(client *tg.Client) *MessageDeleteHandler {
	return &MessageDeleteHandler{client: client}
}

// DeleteMessageInput is the input for the DeleteMessage tool.
//
// MessageID is an opaque handle returned by GetMessages or SendMessage:
//   - "42"   → delete a regular (delivered) message
//   - "s:42" → cancel a pending scheduled message
//
// The handle format tells the server which API to call, so there is no
// round-trip probe to detect the message type.
type DeleteMessageInput struct {
	ChatID    int64  `json:"chat_id" jsonschema:"The ID of the chat containing the message"`
	MessageID string `json:"message_id" jsonschema:"Opaque message handle from GetMessages or SendMessage. \"42\" for a regular message\\, \"s:42\" for a pending scheduled message."`
}

const statusDeleted = "deleted"

// DeleteMessageResult is the typed output of DeleteMessage. The Kind field
// reports whether the deleted message was regular or scheduled, so clients
// can render the right confirmation text without re-parsing the input.
type DeleteMessageResult struct {
	Status        string `json:"status"`
	Kind          string `json:"kind"` // "regular" | "scheduled"
	ChatID        int64  `json:"chat_id"`
	MessageID     string `json:"message_id"`
	Pts           int    `json:"pts,omitempty"`
	RevokedForAll bool   `json:"revoked_for_all,omitempty"`
}

// Register adds the tool to the MCP server.
func (h *MessageDeleteHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "DeleteMessage",
		Description: "Delete a message from a chat. Works for both regular and scheduled messages — the server routes to the correct API based on the opaque handle format (\"42\" regular, \"s:42\" scheduled). Regular deletions remove the message for all participants and cannot be undone. Scheduled deletions cancel pending delivery — the message has not been sent yet. Use GetMessages first (with include_scheduled=true for the scheduled queue) to verify the correct handle.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptrTrue(), OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *MessageDeleteHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in DeleteMessageInput) (*mcp.CallToolResult, *DeleteMessageResult, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}
	ref, err := ParseMessageRef(in.MessageID)
	if err != nil {
		return errInvalidMessageID(in.MessageID, err), nil, nil
	}

	peer, err := tgclient.ResolvePeer(ctx, h.client, in.ChatID)
	if err != nil {
		return errResolvePeer(in.ChatID, err), nil, nil
	}

	if ref.Scheduled {
		return h.deleteScheduled(ctx, req, in.ChatID, ref, peer)
	}
	return h.deleteRegular(ctx, req, in.ChatID, ref, peer)
}

// deleteScheduled cancels a pending scheduled message via
// messages.deleteScheduledMessages. The confirmation wording calls out
// that the message has not been sent yet (so "for all participants" is
// meaningless), and the cancel-path recovery hint points back to
// GetMessages+include_scheduled for re-discovery.
func (h *MessageDeleteHandler) deleteScheduled(ctx context.Context, req *mcp.CallToolRequest, chatID int64, ref MessageRef, peer tg.InputPeerClass) (*mcp.CallToolResult, *DeleteMessageResult, error) {
	confirmed, err := confirmDestructive(ctx, req, fmt.Sprintf(
		"Cancel scheduled message %d in chat %d? It is stored on Telegram's servers and has not been sent yet.",
		ref.ID, chatID,
	))
	if err != nil {
		return errResult(fmt.Sprintf("confirmation failed: %v", err)), nil, nil
	}
	if !confirmed {
		return textResult("Cancelled by user. No scheduled message was deleted. Run GetMessages with include_scheduled=true to view pending messages and retry when ready."), nil, nil
	}

	if _, err := h.client.MessagesDeleteScheduledMessages(ctx, &tg.MessagesDeleteScheduledMessagesRequest{
		Peer: peer,
		ID:   []int{ref.ID},
	}); err != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "DeleteMessage", map[string]any{
			"action":  "delete_scheduled_failed",
			"chat_id": chatID,
			"msg_id":  ref.ID,
			"error":   err.Error(),
		})
		return errResult(fmt.Sprintf("Failed to delete scheduled message: %v", err)), nil, nil
	}
	return nil, &DeleteMessageResult{
		Status:    statusDeleted,
		Kind:      kindScheduled,
		ChatID:    chatID,
		MessageID: ref.Format(),
	}, nil
}

// deleteRegular deletes a delivered message via messages.deleteMessages
// (or channels.deleteMessages for channels/supergroups). Always revokes
// (deletes for all participants) where the API supports it.
func (h *MessageDeleteHandler) deleteRegular(ctx context.Context, req *mcp.CallToolRequest, chatID int64, ref MessageRef, peer tg.InputPeerClass) (*mcp.CallToolResult, *DeleteMessageResult, error) {
	confirmed, err := confirmDestructive(ctx, req, fmt.Sprintf(
		"Delete message %d in chat %d? This deletes for all participants and cannot be undone.",
		ref.ID, chatID,
	))
	if err != nil {
		return errResult(fmt.Sprintf("confirmation failed: %v", err)), nil, nil
	}
	if !confirmed {
		return textResult("Cancelled by user. No message was deleted."), nil, nil
	}

	switch p := peer.(type) {
	case *tg.InputPeerChannel:
		affected, err := h.client.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
			Channel: &tg.InputChannel{
				ChannelID:  p.ChannelID,
				AccessHash: p.AccessHash,
			},
			ID: []int{ref.ID},
		})
		if err != nil {
			mcpLog(ctx, req.Session, logLevelWarning, "DeleteMessage", map[string]any{
				"action":  "delete_channel_message_failed",
				"chat_id": chatID,
				"msg_id":  ref.ID,
				"error":   err.Error(),
			})
			return errResult(fmt.Sprintf("Failed to delete message: %v", err)), nil, nil
		}
		return nil, &DeleteMessageResult{
			Status:        statusDeleted,
			Kind:          kindRegular,
			ChatID:        chatID,
			MessageID:     ref.Format(),
			Pts:           affected.Pts,
			RevokedForAll: true,
		}, nil

	default:
		affected, err := h.client.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{
			Revoke: true,
			ID:     []int{ref.ID},
		})
		if err != nil {
			mcpLog(ctx, req.Session, logLevelWarning, "DeleteMessage", map[string]any{
				"action":  "delete_message_failed",
				"chat_id": chatID,
				"msg_id":  ref.ID,
				"error":   err.Error(),
			})
			return errResult(fmt.Sprintf("Failed to delete message: %v", err)), nil, nil
		}
		return nil, &DeleteMessageResult{
			Status:        statusDeleted,
			Kind:          kindRegular,
			ChatID:        chatID,
			MessageID:     ref.Format(),
			Pts:           affected.Pts,
			RevokedForAll: true,
		}, nil
	}
}
