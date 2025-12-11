package tools

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessageDraftHandler handles the DraftMessage tool
type MessageDraftHandler struct {
	client *tg.Client
}

// NewMessageDraftHandler creates a new MessageDraftHandler
func NewMessageDraftHandler(client *tg.Client) *MessageDraftHandler {
	return &MessageDraftHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *MessageDraftHandler) Tool() mcp.Tool {
	return mcp.NewTool("DraftMessage",
		mcp.WithDescription("Draft a message in a given chat, group or channel. The message will be saved as a draft and can be sent later."),
		mcp.WithNumber("chat_id",
			mcp.Description("The ID of the chat to save the draft to"),
			mcp.Required(),
		),
		mcp.WithString("message",
			mcp.Description("The message text to save as draft"),
			mcp.Required(),
		),
	)
}

// Handle processes the DraftMessage tool request
func (h *MessageDraftHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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

	// Save the draft
	_, err = h.client.MessagesSaveDraft(ctx, &tg.MessagesSaveDraftRequest{
		Peer:    peer,
		Message: message,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to save draft: %v", err)), nil
	}

	result := fmt.Sprintf("Draft message saved successfully for chat %d", chatID)
	return mcp.NewToolResultText(result), nil
}
