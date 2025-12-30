package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessageReplyHandler handles the ReplyToMessage tool
type MessageReplyHandler struct {
	client *tg.Client
}

// NewMessageReplyHandler creates a new MessageReplyHandler
func NewMessageReplyHandler(client *tg.Client) *MessageReplyHandler {
	return &MessageReplyHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *MessageReplyHandler) Tool() mcp.Tool {
	return mcp.NewTool("ReplyToMessage",
		mcp.WithDescription("Reply to a specific message in a chat."),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithNumber("chat_id",
			mcp.Description("The ID of the chat containing the message"),
			mcp.Required(),
		),
		mcp.WithNumber("message_id",
			mcp.Description("The ID of the message to reply to"),
			mcp.Required(),
		),
		mcp.WithString("text",
			mcp.Description("The reply text to send"),
			mcp.Required(),
		),
	)
}

// Handle processes the ReplyToMessage tool request
func (h *MessageReplyHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatID := mcp.ParseInt64(request, "chat_id", 0)
	if chatID == 0 {
		return mcp.NewToolResultError("chat_id is required"), nil
	}

	messageID := mcp.ParseInt(request, "message_id", 0)
	if messageID == 0 {
		return mcp.NewToolResultError("message_id is required"), nil
	}

	text := mcp.ParseString(request, "text", "")
	if text == "" {
		return mcp.NewToolResultError("text is required"), nil
	}

	// Resolve the peer
	peer, err := tgclient.ResolvePeer(ctx, h.client, chatID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to resolve peer: %v", err)), nil
	}

	// Send the reply
	updates, err := h.client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     peer,
		Message:  text,
		RandomID: time.Now().UnixNano(),
		ReplyTo: &tg.InputReplyToMessage{
			ReplyToMsgID: messageID,
		},
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to send reply: %v", err)), nil
	}

	// Extract message ID from updates
	var sentMsgID int
	var date int

	switch u := updates.(type) {
	case *tg.UpdateShortSentMessage:
		sentMsgID = u.ID
		date = u.Date
	case *tg.Updates:
		for _, update := range u.Updates {
			if newMsg, ok := update.(*tg.UpdateNewMessage); ok {
				if msg, ok := newMsg.Message.(*tg.Message); ok {
					sentMsgID = msg.ID
					date = msg.Date
					break
				}
			}
			if newMsg, ok := update.(*tg.UpdateNewChannelMessage); ok {
				if msg, ok := newMsg.Message.(*tg.Message); ok {
					sentMsgID = msg.ID
					date = msg.Date
					break
				}
			}
		}
	}

	result := fmt.Sprintf("Reply sent successfully!\nChat ID: %d\nReplying to message ID: %d\nNew message ID: %d\nDate: %s",
		chatID,
		messageID,
		sentMsgID,
		time.Unix(int64(date), 0).Format(time.RFC3339),
	)

	return mcp.NewToolResultText(result), nil
}
