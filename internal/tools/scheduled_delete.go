package tools

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// ScheduledDeleteHandler handles the DeleteScheduledMessage tool
type ScheduledDeleteHandler struct {
	client *tg.Client
}

// NewScheduledDeleteHandler creates a new ScheduledDeleteHandler
func NewScheduledDeleteHandler(client *tg.Client) *ScheduledDeleteHandler {
	return &ScheduledDeleteHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *ScheduledDeleteHandler) Tool() mcp.Tool {
	return mcp.NewTool("DeleteScheduledMessage",
		mcp.WithDescription("Cancel a scheduled message before it's sent."),
		mcp.WithNumber("chat_id",
			mcp.Description("The ID of the chat containing the scheduled message"),
			mcp.Required(),
		),
		mcp.WithNumber("message_id",
			mcp.Description("The ID of the scheduled message to delete"),
			mcp.Required(),
		),
	)
}

// Handle processes the DeleteScheduledMessage tool request
func (h *ScheduledDeleteHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatID := mcp.ParseInt64(request, "chat_id", 0)
	if chatID == 0 {
		return mcp.NewToolResultError("chat_id is required"), nil
	}

	messageID := mcp.ParseInt(request, "message_id", 0)
	if messageID == 0 {
		return mcp.NewToolResultError("message_id is required"), nil
	}

	// Resolve the peer
	peer, err := tgclient.ResolvePeer(ctx, h.client, chatID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to resolve peer: %v", err)), nil
	}

	// Delete the scheduled message
	_, err = h.client.MessagesDeleteScheduledMessages(ctx, &tg.MessagesDeleteScheduledMessagesRequest{
		Peer: peer,
		ID:   []int{messageID},
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to delete scheduled message: %v", err)), nil
	}

	result := fmt.Sprintf("Scheduled message canceled successfully!\nMessage ID: %d\nChat: %d\n\nThe message has been removed from the schedule queue and will not be sent.",
		messageID, chatID)

	return mcp.NewToolResultText(result), nil
}
