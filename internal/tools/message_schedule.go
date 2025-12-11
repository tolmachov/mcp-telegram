package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessageScheduleHandler handles the ScheduleMessage tool
type MessageScheduleHandler struct {
	client *tg.Client
}

// NewMessageScheduleHandler creates a new MessageScheduleHandler
func NewMessageScheduleHandler(client *tg.Client) *MessageScheduleHandler {
	return &MessageScheduleHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *MessageScheduleHandler) Tool() mcp.Tool {
	return mcp.NewTool("ScheduleMessage",
		mcp.WithDescription("Schedule a message to be sent at a specific time using Telegram's native scheduling API."),
		mcp.WithNumber("chat_id",
			mcp.Description("The ID of the chat to schedule the message for"),
			mcp.Required(),
		),
		mcp.WithString("message",
			mcp.Description("The message text to schedule"),
			mcp.Required(),
		),
		mcp.WithNumber("delay_seconds",
			mcp.Description("Number of seconds from now to send the message"),
			mcp.Required(),
		),
	)
}

// Handle processes the ScheduleMessage tool request
func (h *MessageScheduleHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatID := mcp.ParseInt64(request, "chat_id", 0)
	if chatID == 0 {
		return mcp.NewToolResultError("chat_id is required"), nil
	}

	message := mcp.ParseString(request, "message", "")
	if message == "" {
		return mcp.NewToolResultError("message is required"), nil
	}

	delaySeconds := mcp.ParseInt(request, "delay_seconds", 0)
	if delaySeconds < 0 {
		return mcp.NewToolResultError("delay_seconds must be a positive number"), nil
	}

	// Resolve the peer
	peer, err := tgclient.ResolvePeer(ctx, h.client, chatID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to resolve peer: %v", err)), nil
	}

	// Calculate schedule time
	scheduleTime := time.Now().Add(time.Duration(delaySeconds) * time.Second)
	scheduleTimestamp := int(scheduleTime.Unix())

	// Send the scheduled message
	updates, err := h.client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:         peer,
		Message:      message,
		RandomID:     time.Now().UnixNano(),
		ScheduleDate: scheduleTimestamp,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to schedule message: %v", err)), nil
	}

	// Extract message ID from updates
	var msgID int

	switch u := updates.(type) {
	case *tg.UpdateShortSentMessage:
		msgID = u.ID
	case *tg.Updates:
		for _, update := range u.Updates {
			if newMsg, ok := update.(*tg.UpdateNewScheduledMessage); ok {
				if msg, ok := newMsg.Message.(*tg.Message); ok {
					msgID = msg.ID
					break
				}
			}
		}
	}

	var result string
	if delaySeconds < 10 {
		result = fmt.Sprintf("Message sent immediately (delay was less than 10 seconds)\nMessage ID: %d\nTo: %d",
			msgID, chatID)
	} else {
		result = fmt.Sprintf("Message scheduled successfully!\nScheduled Message ID: %d\nWill be sent at: %s\nTo: %d\nDelay: %d seconds\n\nNote: The message is stored on Telegram's servers and will be sent automatically at the scheduled time, even if you're offline.",
			msgID,
			scheduleTime.Format("2006-01-02 15:04:05"),
			chatID,
			delaySeconds,
		)
	}

	return mcp.NewToolResultText(result), nil
}
