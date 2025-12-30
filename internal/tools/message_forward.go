package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessageForwardHandler handles the ForwardMessage tool
type MessageForwardHandler struct {
	client *tg.Client
}

// NewMessageForwardHandler creates a new MessageForwardHandler
func NewMessageForwardHandler(client *tg.Client) *MessageForwardHandler {
	return &MessageForwardHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *MessageForwardHandler) Tool() mcp.Tool {
	return mcp.NewTool("ForwardMessage",
		mcp.WithDescription("Forward a message from one chat to another."),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithNumber("from_chat_id",
			mcp.Description("The ID of the chat to forward from"),
			mcp.Required(),
		),
		mcp.WithNumber("message_id",
			mcp.Description("The ID of the message to forward"),
			mcp.Required(),
		),
		mcp.WithNumber("to_chat_id",
			mcp.Description("The ID of the chat to forward to"),
			mcp.Required(),
		),
	)
}

// Handle processes the ForwardMessage tool request
func (h *MessageForwardHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	fromChatID := mcp.ParseInt64(request, "from_chat_id", 0)
	if fromChatID == 0 {
		return mcp.NewToolResultError("from_chat_id is required"), nil
	}

	messageID := mcp.ParseInt(request, "message_id", 0)
	if messageID == 0 {
		return mcp.NewToolResultError("message_id is required"), nil
	}

	toChatID := mcp.ParseInt64(request, "to_chat_id", 0)
	if toChatID == 0 {
		return mcp.NewToolResultError("to_chat_id is required"), nil
	}

	// Resolve both peers
	fromPeer, err := tgclient.ResolvePeer(ctx, h.client, fromChatID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to resolve source chat: %v", err)), nil
	}

	toPeer, err := tgclient.ResolvePeer(ctx, h.client, toChatID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to resolve destination chat: %v", err)), nil
	}

	// Forward the message
	updates, err := h.client.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
		FromPeer: fromPeer,
		ID:       []int{messageID},
		ToPeer:   toPeer,
		RandomID: []int64{time.Now().UnixNano()},
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to forward message: %v", err)), nil
	}

	// Extract forwarded message info
	var forwardedMsgID int
	var date int

	switch u := updates.(type) {
	case *tg.Updates:
		for _, update := range u.Updates {
			if newMsg, ok := update.(*tg.UpdateNewMessage); ok {
				if msg, ok := newMsg.Message.(*tg.Message); ok {
					forwardedMsgID = msg.ID
					date = msg.Date
					break
				}
			}
			if newMsg, ok := update.(*tg.UpdateNewChannelMessage); ok {
				if msg, ok := newMsg.Message.(*tg.Message); ok {
					forwardedMsgID = msg.ID
					date = msg.Date
					break
				}
			}
		}
	}

	result := fmt.Sprintf("Message forwarded successfully!\nFrom chat ID: %d\nOriginal message ID: %d\nTo chat ID: %d\nNew message ID: %d\nDate: %s",
		fromChatID,
		messageID,
		toChatID,
		forwardedMsgID,
		time.Unix(int64(date), 0).Format(time.RFC3339),
	)

	return mcp.NewToolResultText(result), nil
}
