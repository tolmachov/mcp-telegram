package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessageReadHandler handles the MarkAsRead tool
type MessageReadHandler struct {
	client *tg.Client
}

// NewMessageReadHandler creates a new MessageReadHandler
func NewMessageReadHandler(client *tg.Client) *MessageReadHandler {
	return &MessageReadHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *MessageReadHandler) Tool() mcp.Tool {
	return mcp.NewTool("MarkAsRead",
		mcp.WithDescription("Mark all messages in one or more chats as read."),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithArray("chat_ids",
			mcp.WithNumberItems(),
			mcp.Description("List of chat IDs to mark as read (max 100)"),
			mcp.Required(),
		),
	)
}

// markReadResult represents the result of marking a single chat as read
type markReadResult struct {
	chatID  int64
	success bool
	err     error
}

// Handle processes the MarkAsRead tool request
func (h *MessageReadHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatIDs := request.GetIntSlice("chat_ids", nil)
	if len(chatIDs) == 0 {
		return mcp.NewToolResultError("chat_ids is required and must not be empty"), nil
	}
	if len(chatIDs) > 100 {
		return mcp.NewToolResultError("Cannot process more than 100 chats at once"), nil
	}

	// Collect results
	results := make([]markReadResult, 0, len(chatIDs))

	// Process sequentially
	for _, cid := range chatIDs {
		chatID := int64(cid)
		err := h.markChatAsRead(ctx, chatID)

		results = append(results, markReadResult{
			chatID:  chatID,
			success: err == nil,
			err:     err,
		})
		// Continue even on error
	}

	return h.formatResult(results), nil
}

// markChatAsRead marks a single chat as read
func (h *MessageReadHandler) markChatAsRead(ctx context.Context, chatID int64) error {
	// Resolve the peer
	peer, err := tgclient.ResolvePeer(ctx, h.client, chatID)
	if err != nil {
		return fmt.Errorf("failed to resolve peer: %w", err)
	}

	// Check if it's a channel
	switch p := peer.(type) {
	case *tg.InputPeerChannel:
		// For channels, use channels.readHistory
		_, err = h.client.ChannelsReadHistory(ctx, &tg.ChannelsReadHistoryRequest{
			Channel: &tg.InputChannel{
				ChannelID:  p.ChannelID,
				AccessHash: p.AccessHash,
			},
		})
		if err != nil {
			return fmt.Errorf("failed to mark channel as read: %w", err)
		}
	default:
		// For private chats and groups, use messages.readHistory
		_, err = h.client.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{
			Peer: peer,
		})
		if err != nil {
			return fmt.Errorf("failed to mark chat as read: %w", err)
		}
	}

	return nil
}

// formatResult formats the batch processing results into a user-friendly message
func (h *MessageReadHandler) formatResult(results []markReadResult) *mcp.CallToolResult {
	successful := 0
	failed := 0

	var successIDs []int64
	var failures []string

	for _, r := range results {
		if r.success {
			successful++
			successIDs = append(successIDs, r.chatID)
		} else {
			failed++
			failures = append(failures, fmt.Sprintf("  - Chat %d: %v", r.chatID, r.err))
		}
	}

	// Build message
	var msg strings.Builder
	msg.WriteString(fmt.Sprintf("Marked %d out of %d chats as read successfully!\n\n",
		successful, len(results)))

	if len(successIDs) > 0 {
		msg.WriteString("Successful:\n")
		for _, id := range successIDs {
			msg.WriteString(fmt.Sprintf("  - Chat %d\n", id))
		}
	}

	if len(failures) > 0 {
		msg.WriteString("\nFailed:\n")
		for _, f := range failures {
			msg.WriteString(f + "\n")
		}
	}

	return mcp.NewToolResultText(msg.String())
}
