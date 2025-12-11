package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// DefaultBackupDir returns the default backup directory based on the OS.
func DefaultBackupDir() string {
	homeDir, _ := os.UserHomeDir()

	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(homeDir, "Library", "Application Support", "mcp-telegram", "backups")
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "mcp-telegram", "backups")
		}
		return filepath.Join(homeDir, "AppData", "Roaming", "mcp-telegram", "backups")
	default: // linux and others
		if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
			return filepath.Join(xdgData, "mcp-telegram", "backups")
		}
		return filepath.Join(homeDir, ".local", "share", "mcp-telegram", "backups")
	}
}

// sanitizeFilename removes or replaces characters that are invalid in filenames.
func sanitizeFilename(name string) string {
	// Replace invalid characters with underscore
	invalid := []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|", "\n", "\r", "\t"}
	result := name
	for _, char := range invalid {
		result = strings.ReplaceAll(result, char, "_")
	}
	// Trim spaces and dots from edges
	result = strings.Trim(result, " .")
	// Limit length
	if len(result) > 100 {
		result = result[:100]
	}
	if result == "" {
		result = "backup"
	}
	return result
}

// isPathAllowed checks if the given path is within one of the allowed directories.
func isPathAllowed(targetPath string, allowedPaths []string) error {
	// Clean and resolve the target path to absolute
	absTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return fmt.Errorf("resolving path: %w", err)
	}
	absTarget = filepath.Clean(absTarget)

	for _, allowed := range allowedPaths {
		absAllowed, err := filepath.Abs(allowed)
		if err != nil {
			continue
		}
		absAllowed = filepath.Clean(absAllowed)

		// Check if target is within the allowed directory
		rel, err := filepath.Rel(absAllowed, absTarget)
		if err != nil {
			continue
		}

		// If rel doesn't start with "..", it's within the allowed directory
		if !strings.HasPrefix(rel, "..") {
			return nil
		}
	}

	return fmt.Errorf("path %q is not within allowed directories. Configure --allowed-paths or TELEGRAM_ALLOWED_PATHS", targetPath)
}

// getChatName returns the name of the chat based on peer type.
func getChatName(ctx context.Context, raw *tg.Client, peer tg.InputPeerClass, chatID int64) string {
	switch p := peer.(type) {
	case *tg.InputPeerUser:
		users, err := raw.UsersGetUsers(ctx, []tg.InputUserClass{
			&tg.InputUser{UserID: p.UserID, AccessHash: p.AccessHash},
		})
		if err == nil && len(users) > 0 {
			if user, ok := users[0].(*tg.User); ok {
				return tgclient.UserName(user)
			}
		}
	case *tg.InputPeerChat:
		chats, err := raw.MessagesGetChats(ctx, []int64{p.ChatID})
		if err == nil {
			if result, ok := chats.(*tg.MessagesChats); ok && len(result.Chats) > 0 {
				if chat, ok := result.Chats[0].(*tg.Chat); ok {
					return chat.Title
				}
			}
		}
	case *tg.InputPeerChannel:
		chats, err := raw.ChannelsGetChannels(ctx, []tg.InputChannelClass{
			&tg.InputChannel{ChannelID: p.ChannelID, AccessHash: p.AccessHash},
		})
		if err == nil {
			if result, ok := chats.(*tg.MessagesChats); ok && len(result.Chats) > 0 {
				if channel, ok := result.Chats[0].(*tg.Channel); ok {
					return channel.Title
				}
			}
		}
	}
	return fmt.Sprintf("chat_%d", chatID)
}

// MessageBackupHandler handles the BackupMessages tool
type MessageBackupHandler struct {
	client       *tg.Client
	allowedPaths []string
}

// NewMessageBackupHandler creates a new MessageBackupHandler
func NewMessageBackupHandler(client *tg.Client, allowedPaths []string) *MessageBackupHandler {
	return &MessageBackupHandler{client: client, allowedPaths: allowedPaths}
}

// Tool returns the MCP tool definition
func (h *MessageBackupHandler) Tool() mcp.Tool {
	return mcp.NewTool("BackupMessages",
		mcp.WithDescription("Backup messages from a chat to a text file. Messages are saved with ID, timestamp, sender name, and reply info. If filepath is not specified, generates automatic filename like 'ChatName-2024-01-15.txt' in default backup directory."),
		mcp.WithNumber("chat_id",
			mcp.Description("The ID of the chat to backup messages from"),
			mcp.Required(),
		),
		mcp.WithString("filepath",
			mcp.Description("Path to the file where messages will be saved (optional, auto-generated if not provided)"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of messages to backup (default: 100)"),
		),
		mcp.WithNumber("offset_id",
			mcp.Description("Message ID from which to start fetching (exclusive, for pagination)"),
		),
	)
}

// Handle processes the BackupMessages tool request
func (h *MessageBackupHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatID := mcp.ParseInt64(request, "chat_id", 0)
	if chatID == 0 {
		return mcp.NewToolResultError("chat_id is required"), nil
	}

	targetPath := mcp.ParseString(request, "filepath", "")
	limit := mcp.ParseInt(request, "limit", 100)
	offsetID := mcp.ParseInt(request, "offset_id", 0)

	// Resolve the peer
	peer, err := tgclient.ResolvePeer(ctx, h.client, chatID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to resolve peer: %v", err)), nil
	}

	// Generate filename if not provided
	if targetPath == "" {
		chatName := getChatName(ctx, h.client, peer, chatID)
		filename := fmt.Sprintf("%s-%s.txt", sanitizeFilename(chatName), time.Now().Format("2006-01-02_15-04-05"))
		targetPath = filepath.Join(h.allowedPaths[0], filename)
	}

	// Validate path against allowed directories
	if err := isPathAllowed(targetPath, h.allowedPaths); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Get message history
	historyRequest := &tg.MessagesGetHistoryRequest{
		Peer:     peer,
		Limit:    limit,
		OffsetID: offsetID,
	}

	history, err := h.client.MessagesGetHistory(ctx, historyRequest)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get messages: %v", err)), nil
	}

	// Extract messages and users
	var messages []tg.MessageClass
	var users []tg.UserClass
	var chats []tg.ChatClass

	switch h := history.(type) {
	case *tg.MessagesMessages:
		messages = h.Messages
		users = h.Users
		chats = h.Chats
	case *tg.MessagesMessagesSlice:
		messages = h.Messages
		users = h.Users
		chats = h.Chats
	case *tg.MessagesChannelMessages:
		messages = h.Messages
		users = h.Users
		chats = h.Chats
	default:
		return mcp.NewToolResultError("Unexpected response type"), nil
	}

	// Build user/chat maps for name lookup
	userMap := make(map[int64]string)
	for _, u := range users {
		if user, ok := u.(*tg.User); ok {
			name := tgclient.UserName(user)
			userMap[user.ID] = name
		}
	}

	chatMap := make(map[int64]string)
	for _, c := range chats {
		switch chat := c.(type) {
		case *tg.Chat:
			chatMap[chat.ID] = chat.Title
		case *tg.Channel:
			chatMap[chat.ID] = chat.Title
		}
	}

	// Format messages for backup
	var lines []string
	for _, msgClass := range messages {
		msg, ok := msgClass.(*tg.Message)
		if !ok {
			continue
		}

		// Skip empty messages (could be service messages)
		if msg.Message == "" {
			continue
		}

		// Format timestamp
		timestamp := time.Unix(int64(msg.Date), 0).Format("2006-01-02 15:04:05")

		// Get sender name
		senderName := "Unknown"
		if msg.FromID != nil {
			switch from := msg.FromID.(type) {
			case *tg.PeerUser:
				if name, ok := userMap[from.UserID]; ok {
					senderName = name
				}
			case *tg.PeerChannel:
				if name, ok := chatMap[from.ChannelID]; ok {
					senderName = name
				}
			case *tg.PeerChat:
				if name, ok := chatMap[from.ChatID]; ok {
					senderName = name
				}
			}
		}

		// Build header: timestamp, sender, then metadata
		header := fmt.Sprintf("[%s] [%s] [id=%d]", timestamp, senderName, msg.ID)

		if msg.ReplyTo != nil {
			if reply, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok {
				if reply.ReplyToMsgID != 0 {
					header += fmt.Sprintf(" [reply_to=%d]", reply.ReplyToMsgID)
				}
			}
		}

		// Add message
		lines = append(lines, "-----")
		lines = append(lines, header)
		lines = append(lines, msg.Message)
	}

	if len(lines) > 0 {
		lines = append(lines, "-----")
	}

	// Ensure parent directory exists
	parentDir := filepath.Dir(targetPath)
	if err := os.MkdirAll(parentDir, 0o750); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to create directory: %v", err)), nil
	}

	// Write to file
	content := strings.Join(lines, "\n")
	err = os.WriteFile(targetPath, []byte(content), 0o600)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to write file: %v", err)), nil
	}

	// Get absolute path for clear output
	absPath, _ := filepath.Abs(targetPath)

	msgCount := len(messages)
	result := fmt.Sprintf("Backup completed!\nMessages saved: %d\nFile: %s", msgCount, absPath)

	return mcp.NewToolResultText(result), nil
}
