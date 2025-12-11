package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// ChatMuteHandler handles the MuteChat tool
type ChatMuteHandler struct {
	client *tg.Client
}

// NewChatMuteHandler creates a new ChatMuteHandler
func NewChatMuteHandler(client *tg.Client) *ChatMuteHandler {
	return &ChatMuteHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *ChatMuteHandler) Tool() mcp.Tool {
	return mcp.NewTool("MuteChat",
		mcp.WithDescription("Mute notifications for a chat."),
		mcp.WithNumber("chat_id",
			mcp.Description("The ID of the chat to mute"),
			mcp.Required(),
		),
		mcp.WithNumber("duration",
			mcp.Description("Duration in seconds (0 = forever, default: forever)"),
		),
	)
}

// Handle processes the MuteChat tool request
func (h *ChatMuteHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatID := mcp.ParseInt64(request, "chat_id", 0)
	if chatID == 0 {
		return mcp.NewToolResultError("chat_id is required"), nil
	}

	// Duration in seconds, 0 = forever
	duration := mcp.ParseInt(request, "duration", 0)

	// Resolve the peer
	peer, err := tgclient.ResolvePeer(ctx, h.client, chatID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to resolve peer: %v", err)), nil
	}

	// Convert InputPeer to InputNotifyPeer
	var notifyPeer tg.InputNotifyPeerClass
	switch p := peer.(type) {
	case *tg.InputPeerUser:
		notifyPeer = &tg.InputNotifyPeer{
			Peer: &tg.InputPeerUser{UserID: p.UserID, AccessHash: p.AccessHash},
		}
	case *tg.InputPeerChat:
		notifyPeer = &tg.InputNotifyPeer{
			Peer: &tg.InputPeerChat{ChatID: p.ChatID},
		}
	case *tg.InputPeerChannel:
		notifyPeer = &tg.InputNotifyPeer{
			Peer: &tg.InputPeerChannel{ChannelID: p.ChannelID, AccessHash: p.AccessHash},
		}
	default:
		return mcp.NewToolResultError("Unsupported peer type"), nil
	}

	// Set mute_until: 0 = default, max int32 = forever, or a specific Unix timestamp
	var muteUntil int
	if duration == 0 {
		// Mute forever (max int32 value)
		muteUntil = 2147483647
	} else {
		// Mute until specific time (current Unix timestamp + duration in seconds)
		muteUntil = int(time.Now().Unix()) + duration
	}

	// Update notification settings
	_, err = h.client.AccountUpdateNotifySettings(ctx, &tg.AccountUpdateNotifySettingsRequest{
		Peer: notifyPeer,
		Settings: tg.InputPeerNotifySettings{
			MuteUntil: muteUntil,
		},
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to mute chat: %v", err)), nil
	}

	var result string
	if duration == 0 {
		result = fmt.Sprintf("Chat %d muted forever", chatID)
	} else {
		result = fmt.Sprintf("Chat %d muted for %d seconds", chatID, duration)
	}

	return mcp.NewToolResultText(result), nil
}

// ChatUnmuteHandler handles the UnmuteChat tool
type ChatUnmuteHandler struct {
	client *tg.Client
}

// NewChatUnmuteHandler creates a new ChatUnmuteHandler
func NewChatUnmuteHandler(client *tg.Client) *ChatUnmuteHandler {
	return &ChatUnmuteHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *ChatUnmuteHandler) Tool() mcp.Tool {
	return mcp.NewTool("UnmuteChat",
		mcp.WithDescription("Unmute notifications for a chat."),
		mcp.WithNumber("chat_id",
			mcp.Description("The ID of the chat to unmute"),
			mcp.Required(),
		),
	)
}

// Handle processes the UnmuteChat tool request
func (h *ChatUnmuteHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatID := mcp.ParseInt64(request, "chat_id", 0)
	if chatID == 0 {
		return mcp.NewToolResultError("chat_id is required"), nil
	}

	// Resolve the peer
	peer, err := tgclient.ResolvePeer(ctx, h.client, chatID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to resolve peer: %v", err)), nil
	}

	// Convert InputPeer to InputNotifyPeer
	var notifyPeer tg.InputNotifyPeerClass
	switch p := peer.(type) {
	case *tg.InputPeerUser:
		notifyPeer = &tg.InputNotifyPeer{
			Peer: &tg.InputPeerUser{UserID: p.UserID, AccessHash: p.AccessHash},
		}
	case *tg.InputPeerChat:
		notifyPeer = &tg.InputNotifyPeer{
			Peer: &tg.InputPeerChat{ChatID: p.ChatID},
		}
	case *tg.InputPeerChannel:
		notifyPeer = &tg.InputNotifyPeer{
			Peer: &tg.InputPeerChannel{ChannelID: p.ChannelID, AccessHash: p.AccessHash},
		}
	default:
		return mcp.NewToolResultError("Unsupported peer type"), nil
	}

	// Reset notification settings (mute_until = 0 means use default/unmuted)
	_, err = h.client.AccountUpdateNotifySettings(ctx, &tg.AccountUpdateNotifySettingsRequest{
		Peer: notifyPeer,
		Settings: tg.InputPeerNotifySettings{
			MuteUntil: 0,
		},
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to unmute chat: %v", err)), nil
	}

	return mcp.NewToolResultText(fmt.Sprintf("Chat %d unmuted", chatID)), nil
}
