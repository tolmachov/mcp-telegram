package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessageSendHandler handles the SendMessage tool
type MessageSendHandler struct {
	client *tg.Client
}

// NewMessageSendHandler creates a new MessageSendHandler
func NewMessageSendHandler(client *tg.Client) *MessageSendHandler {
	return &MessageSendHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *MessageSendHandler) Tool() mcp.Tool {
	return mcp.NewTool("SendMessage",
		mcp.WithDescription("Send a message to a contact, group, or channel."),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithNumber("chat_id",
			mcp.Description("The ID of the chat to send the message to"),
			mcp.Required(),
		),
		mcp.WithString("message",
			mcp.Description("The message text to send"),
			mcp.Required(),
		),
	)
}

// Handle processes the SendMessage tool request
func (h *MessageSendHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatID := mcp.ParseInt64(request, "chat_id", 0)
	if chatID == 0 {
		return mcp.NewToolResultError("chat_id is required"), nil
	}

	message := mcp.ParseString(request, "message", "")
	if message == "" {
		return mcp.NewToolResultError("message is required"), nil
	}

	// Resolve the peer
	peer, err := tgclient.ResolvePeer(ctx, h.client, chatID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to resolve peer: %v", err)), nil
	}

	// Send the message
	updates, err := h.client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     peer,
		Message:  message,
		RandomID: time.Now().UnixNano(),
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to send message: %v", err)), nil
	}

	// Extract message ID from updates
	var msgID int
	var date int

	switch u := updates.(type) {
	case *tg.UpdateShortSentMessage:
		msgID = u.ID
		date = u.Date
	case *tg.Updates:
		for _, update := range u.Updates {
			if newMsg, ok := update.(*tg.UpdateNewMessage); ok {
				if msg, ok := newMsg.Message.(*tg.Message); ok {
					msgID = msg.ID
					date = msg.Date
					break
				}
			}
			if newMsg, ok := update.(*tg.UpdateNewChannelMessage); ok {
				if msg, ok := newMsg.Message.(*tg.Message); ok {
					msgID = msg.ID
					date = msg.Date
					break
				}
			}
		}
	}

	result := fmt.Sprintf("Message sent successfully!\nMessage ID: %d\nDate: %s\nTo: %d",
		msgID,
		time.Unix(int64(date), 0).Format(time.RFC3339),
		chatID,
	)

	return mcp.NewToolResultText(result), nil
}
