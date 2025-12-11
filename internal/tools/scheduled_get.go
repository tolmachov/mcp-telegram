package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// ScheduledGetHandler handles the GetScheduledMessages tool
type ScheduledGetHandler struct {
	client *tg.Client
}

// NewScheduledGetHandler creates a new ScheduledGetHandler
func NewScheduledGetHandler(client *tg.Client) *ScheduledGetHandler {
	return &ScheduledGetHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *ScheduledGetHandler) Tool() mcp.Tool {
	return mcp.NewTool("GetScheduledMessages",
		mcp.WithDescription("Get all scheduled messages for a specific chat from Telegram's schedule queue."),
		mcp.WithNumber("chat_id",
			mcp.Description("The ID of the chat to get scheduled messages from"),
			mcp.Required(),
		),
	)
}

// Handle processes the GetScheduledMessages tool request
func (h *ScheduledGetHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatID := mcp.ParseInt64(request, "chat_id", 0)
	if chatID == 0 {
		return mcp.NewToolResultError("chat_id is required"), nil
	}

	// Resolve the peer
	peer, err := tgclient.ResolvePeer(ctx, h.client, chatID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to resolve peer: %v", err)), nil
	}

	// Get scheduled messages
	scheduled, err := h.client.MessagesGetScheduledHistory(ctx, &tg.MessagesGetScheduledHistoryRequest{
		Peer: peer,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get scheduled messages: %v", err)), nil
	}

	var messages []tg.MessageClass

	switch s := scheduled.(type) {
	case *tg.MessagesMessages:
		messages = s.Messages
	case *tg.MessagesMessagesSlice:
		messages = s.Messages
	case *tg.MessagesChannelMessages:
		messages = s.Messages
	default:
		return mcp.NewToolResultError("Unexpected response type"), nil
	}

	if len(messages) == 0 {
		return mcp.NewToolResultText(fmt.Sprintf("No scheduled messages found for chat %d", chatID)), nil
	}

	var results []string
	results = append(results, fmt.Sprintf("Scheduled Messages (%d total):", len(messages)))

	for _, msgClass := range messages {
		msg, ok := msgClass.(*tg.Message)
		if !ok {
			continue
		}

		scheduleTime := time.Unix(int64(msg.Date), 0)

		result := fmt.Sprintf("\n* Message ID: %d\n  Scheduled for: %s\n  Text: %s",
			msg.ID,
			scheduleTime.Format("2006-01-02 15:04:05"),
			truncateRunes(msg.Message, 100),
		)
		results = append(results, result)
	}

	return mcp.NewToolResultText(strings.Join(results, "\n")), nil
}
