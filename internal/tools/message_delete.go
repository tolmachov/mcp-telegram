package tools

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessageDeleteHandler handles the DeleteMessage tool
type MessageDeleteHandler struct {
	client *tg.Client
}

// NewMessageDeleteHandler creates a new MessageDeleteHandler
func NewMessageDeleteHandler(client *tg.Client) *MessageDeleteHandler {
	return &MessageDeleteHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *MessageDeleteHandler) Tool() mcp.Tool {
	return mcp.NewTool("DeleteMessage",
		mcp.WithDescription("Delete a message from a chat. This action cannot be undone. For non-channel chats, the message will be deleted for all participants."),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithNumber("chat_id",
			mcp.Description("The ID of the chat containing the message"),
			mcp.Required(),
		),
		mcp.WithNumber("message_id",
			mcp.Description("The ID of the message to delete"),
			mcp.Required(),
		),
	)
}

// Handle processes the DeleteMessage tool request
func (h *MessageDeleteHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatID := mcp.ParseInt64(request, "chat_id", 0)
	if chatID == 0 {
		return mcp.NewToolResultError("chat_id is required"), nil
	}

	messageID := mcp.ParseInt(request, "message_id", 0)
	if messageID == 0 {
		return mcp.NewToolResultError("message_id is required"), nil
	}

	// Always revoke (delete for all participants)
	revoke := true

	// Resolve the peer
	peer, err := tgclient.ResolvePeer(ctx, h.client, chatID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to resolve peer: %v", err)), nil
	}

	// Check if it's a channel
	switch p := peer.(type) {
	case *tg.InputPeerChannel:
		// For channels, use channels.deleteMessages
		affected, err := h.client.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
			Channel: &tg.InputChannel{
				ChannelID:  p.ChannelID,
				AccessHash: p.AccessHash,
			},
			ID: []int{messageID},
		})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to delete message: %v", err)), nil
		}

		result := fmt.Sprintf("Message deleted successfully!\nChat ID: %d\nMessage ID: %d\nMessages affected: %d",
			chatID, messageID, affected.Pts)
		return mcp.NewToolResultText(result), nil

	default:
		// For private chats and groups, use messages.deleteMessages
		affected, err := h.client.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{
			Revoke: revoke,
			ID:     []int{messageID},
		})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to delete message: %v", err)), nil
		}

		result := fmt.Sprintf("Message deleted successfully!\nChat ID: %d\nMessage ID: %d\nMessages affected: %d\nRevoked for all: %t",
			chatID, messageID, affected.Pts, revoke)
		return mcp.NewToolResultText(result), nil
	}
}
