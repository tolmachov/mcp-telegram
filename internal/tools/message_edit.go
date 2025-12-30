package tools

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessageEditHandler handles the EditMessage tool
type MessageEditHandler struct {
	client *tg.Client
}

// NewMessageEditHandler creates a new MessageEditHandler
func NewMessageEditHandler(client *tg.Client) *MessageEditHandler {
	return &MessageEditHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *MessageEditHandler) Tool() mcp.Tool {
	return mcp.NewTool("EditMessage",
		mcp.WithDescription("Edit a message you previously sent."),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithNumber("chat_id",
			mcp.Description("The ID of the chat containing the message"),
			mcp.Required(),
		),
		mcp.WithNumber("message_id",
			mcp.Description("The ID of the message to edit"),
			mcp.Required(),
		),
		mcp.WithString("new_text",
			mcp.Description("The new text for the message"),
			mcp.Required(),
		),
	)
}

// Handle processes the EditMessage tool request
func (h *MessageEditHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatID := mcp.ParseInt64(request, "chat_id", 0)
	if chatID == 0 {
		return mcp.NewToolResultError("chat_id is required"), nil
	}

	messageID := mcp.ParseInt(request, "message_id", 0)
	if messageID == 0 {
		return mcp.NewToolResultError("message_id is required"), nil
	}

	newText := mcp.ParseString(request, "new_text", "")
	if newText == "" {
		return mcp.NewToolResultError("new_text is required"), nil
	}

	// Resolve the peer
	peer, err := tgclient.ResolvePeer(ctx, h.client, chatID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to resolve peer: %v", err)), nil
	}

	// Edit the message
	updates, err := h.client.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{
		Peer:    peer,
		ID:      messageID,
		Message: newText,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to edit message: %v", err)), nil
	}

	// Extract updated message info
	var editedMsgID int
	var date int

	switch u := updates.(type) {
	case *tg.Updates:
		for _, update := range u.Updates {
			if editMsg, ok := update.(*tg.UpdateEditMessage); ok {
				if msg, ok := editMsg.Message.(*tg.Message); ok {
					editedMsgID = msg.ID
					date = msg.Date
					break
				}
			}
			if editMsg, ok := update.(*tg.UpdateEditChannelMessage); ok {
				if msg, ok := editMsg.Message.(*tg.Message); ok {
					editedMsgID = msg.ID
					date = msg.Date
					break
				}
			}
		}
	}

	result := fmt.Sprintf("Message edited successfully!\nChat ID: %d\nMessage ID: %d\nUpdated text: %s",
		chatID, editedMsgID, newText)

	if date > 0 {
		result += fmt.Sprintf("\nEdit time: %d", date)
	}

	return mcp.NewToolResultText(result), nil
}
