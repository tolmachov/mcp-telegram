package tools

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/presentation"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessageForwardHandler handles the ForwardMessage tool.
type MessageForwardHandler struct {
	client *tg.Client
	peers  *tgclient.Resolver
}

// NewMessageForwardHandler creates a new MessageForwardHandler.
func NewMessageForwardHandler(peers *tgclient.Resolver) *MessageForwardHandler {
	return &MessageForwardHandler{client: peers.Client(), peers: peers}
}

// ForwardMessageInput is the input for the ForwardMessage tool.
//
// MessageID is an opaque handle; scheduled handles ("s:...") are rejected
// because scheduled messages haven't been delivered yet and cannot be
// forwarded.
type ForwardMessageInput struct {
	FromChatID int64  `json:"from_chat_id" jsonschema:"The ID of the chat to forward from"`
	MessageID  string `json:"message_id" jsonschema:"Opaque message handle from GetMessages. Scheduled handles (\"s:...\") are not supported."`
	ToChatID   int64  `json:"to_chat_id" jsonschema:"The ID of the chat to forward to"`
	Confirm    bool   `json:"confirm" jsonschema:"Must be true after the user explicitly confirms publishing the copy to the destination chat"`
}

const statusForwarded = "forwarded"

// ForwardMessageResult is the typed output of ForwardMessage.
type ForwardMessageResult struct {
	Status            string `json:"status"` // "forwarded"
	FromChatID        int64  `json:"from_chat_id"`
	OriginalMessageID string `json:"original_message_id"`
	ToChatID          int64  `json:"to_chat_id"`
	NewMessageID      string `json:"new_message_id,omitempty"`
	Date              string `json:"date,omitempty"`
	Note              string `json:"note,omitempty"`
}

// Register adds the tool to the MCP server.
func (h *MessageForwardHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name:        "ForwardMessage",
		Description: "Copy a message from one chat to another, preserving the original sender attribution and any media. Works with any regular message type (text, media, documents); scheduled handles (\"s:...\") are rejected. Both chats must be accessible to you. The copy is a new, independent message visible to all members of the destination chat — it does not carry the original send time. The call is rejected unless confirm=true. To send fresh text instead of copying, use SendMessage.",
		// DestructiveHint mirrors the other confirm-gated tools (DeleteMessages,
		// LeaveChat): forwarding publishes content into another chat, an
		// outward-facing side effect the handler asks to confirm.
		Annotations: &mcp.ToolAnnotations{DestructiveHint: new(true), OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *MessageForwardHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in ForwardMessageInput) (*mcp.CallToolResult, *ForwardMessageResult, error) {
	if in.FromChatID == 0 {
		return errResult("from_chat_id is required. Use SearchChats or GetChats to find the source chat ID."), nil, nil
	}
	msgID, errRes := parseRegularRef("message_id", in.MessageID, "forward")
	if errRes != nil {
		return errRes, nil, nil
	}
	if in.ToChatID == 0 {
		return errResult("to_chat_id is required. Use SearchChats or GetChats to find the destination chat ID."), nil, nil
	}
	if errRes := requireExplicitConfirmation(in.Confirm, "forward the message"); errRes != nil {
		return errRes, nil, nil
	}

	// A bad chat ID surfaces as a resolve error before the mutation is issued.
	randomID := cryptoRandInt64()
	updates, err := tgclient.WithPeers(ctx, h.peers, []int64{in.FromChatID, in.ToChatID}, nil, nil, func(p []tgclient.Peer) (tg.UpdatesClass, error) {
		return h.client.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
			FromPeer: p[0].Input,
			ID:       []int{msgID},
			ToPeer:   p[1].Input,
			RandomID: []int64{randomID},
		})
	})
	if err != nil {
		return nil, nil, failed(fmt.Sprintf("forward message %s from chat %d to chat %d", in.MessageID, in.FromChatID, in.ToChatID), err)
	}

	forwardedMsgID, date := extractSentMessageID(updates)
	if forwardedMsgID == 0 {
		mcpLog(ctx, req.Session, logLevelWarning, "ForwardMessage", map[string]any{
			"action": "message_id_extraction_failed",
			"note":   "Telegram returned an unrecognised update type; new_message_id in response is unreliable",
		})
	}
	res := &ForwardMessageResult{
		Status:            statusForwarded,
		FromChatID:        in.FromChatID,
		OriginalMessageID: presentation.FormatRegularRef(msgID),
		ToChatID:          in.ToChatID,
	}
	if forwardedMsgID > 0 {
		res.NewMessageID = presentation.FormatRegularRef(forwardedMsgID)
	} else {
		res.Note = "new_message_id unavailable: Telegram returned an unrecognised update type"
	}
	if date > 0 {
		res.Date = formatUnixRFC3339(date)
	}
	return nil, res, nil
}
