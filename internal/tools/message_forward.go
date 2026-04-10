package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessageForwardHandler handles the ForwardMessage tool.
type MessageForwardHandler struct {
	client *tg.Client
}

// NewMessageForwardHandler creates a new MessageForwardHandler.
func NewMessageForwardHandler(client *tg.Client) *MessageForwardHandler {
	return &MessageForwardHandler{client: client}
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
}

const statusForwarded = "forwarded"

// ForwardMessageResult is the typed output of ForwardMessage.
type ForwardMessageResult struct {
	Status            string `json:"status"`
	FromChatID        int64  `json:"from_chat_id"`
	OriginalMessageID string `json:"original_message_id"`
	ToChatID          int64  `json:"to_chat_id"`
	NewMessageID      string `json:"new_message_id,omitempty"`
	Date              string `json:"date,omitempty"`
	Note              string `json:"note,omitempty"`
}

// Register adds the tool to the MCP server.
func (h *MessageForwardHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ForwardMessage",
		Description: "Forward a message from one chat to another. The forwarded message retains the original sender attribution.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *MessageForwardHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in ForwardMessageInput) (*mcp.CallToolResult, *ForwardMessageResult, error) {
	if in.FromChatID == 0 {
		return errResult("from_chat_id is required. Use SearchChats or GetChats to find the source chat ID."), nil, nil
	}
	ref, err := ParseMessageRef(in.MessageID)
	if err != nil {
		return errInvalidMessageID(in.MessageID, err), nil, nil
	}
	if ref.Scheduled {
		return errCannotOnScheduled("forward"), nil, nil
	}
	if in.ToChatID == 0 {
		return errResult("to_chat_id is required. Use SearchChats or GetChats to find the destination chat ID."), nil, nil
	}

	fromPeer, err := tgclient.ResolvePeer(ctx, h.client, in.FromChatID)
	if err != nil {
		return errResolvePeer(in.FromChatID, err), nil, nil
	}
	toPeer, err := tgclient.ResolvePeer(ctx, h.client, in.ToChatID)
	if err != nil {
		return errResolvePeer(in.ToChatID, err), nil, nil
	}

	updates, err := h.client.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
		FromPeer: fromPeer,
		ID:       []int{ref.ID},
		ToPeer:   toPeer,
		RandomID: []int64{cryptoRandInt64()},
	})
	if err != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "ForwardMessage", map[string]any{
			"action":   "forward_failed",
			"from_id":  in.FromChatID,
			"to_id":    in.ToChatID,
			"msg_id":   ref.ID,
			"error":    err.Error(),
		})
		return errResult(fmt.Sprintf("Failed to forward message: %v", err)), nil, nil
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
		OriginalMessageID: ref.Format(),
		ToChatID:          in.ToChatID,
	}
	if forwardedMsgID > 0 {
		res.NewMessageID = FormatRegularRef(forwardedMsgID)
	} else {
		res.Note = "new_message_id unavailable: Telegram returned an unrecognised update type"
	}
	if date > 0 {
		res.Date = time.Unix(int64(date), 0).UTC().Format(time.RFC3339)
	}
	return nil, res, nil
}
