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
	"github.com/mark3labs/mcp-go/server"
	"go.uber.org/ratelimit"

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

		// Check if the target is within the allowed directory
		rel, err := filepath.Rel(absAllowed, absTarget)
		if err != nil {
			continue
		}

		// If rel doesn't start with, "..", it's within the allowed directory
		if !strings.HasPrefix(rel, "..") {
			return nil
		}
	}

	return fmt.Errorf("path %q is not within allowed directories. Configure --allowed-paths or TELEGRAM_ALLOWED_PATHS", targetPath)
}

// getChatName returns the name of the chat based on a peer type.
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
		mcp.WithDescription("Backup messages from a chat to a text file. Messages are saved with timestamp, sender name, ID, and reply info. If filepath is not specified, generates automatic filename like 'ChatName-2024-01-15.txt' in default backup directory. All filter parameters are optional - if none specified, backs up last 1000 messages."),
		mcp.WithNumber("chat_id",
			mcp.Description("The ID of the chat to backup messages from"),
			mcp.Required(),
		),
		mcp.WithString("filepath",
			mcp.Description("Path to the file where messages will be saved (optional, auto-generated if not provided)"),
		),
		mcp.WithNumber("count",
			mcp.Description("Maximum number of messages to backup (optional, default: 1000 if no filters specified)"),
		),
		mcp.WithString("from",
			mcp.Description("Start date - backup messages from this date (optional, format: YYYY-MM-DD or YYYY-MM-DD HH:MM:SS)"),
		),
		mcp.WithString("to",
			mcp.Description("End date - backup messages until this date (optional, format: YYYY-MM-DD or YYYY-MM-DD HH:MM:SS)"),
		),
	)
}

// parseDate parses a date string in format YYYY-MM-DD or YYYY-MM-DD HH:MM:SS
func parseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	// Try the full datetime format first
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.Local)
	if err == nil {
		return t, nil
	}
	// Try a date-only format
	t, err = time.ParseInLocation("2006-01-02", s, time.Local)
	if err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid date format %q, expected YYYY-MM-DD or YYYY-MM-DD HH:MM:SS", s)
}

// backupProgress handles progress tracking and notifications for message backup
type backupProgress struct {
	ctx           context.Context
	srv           *server.MCPServer
	progressToken mcp.ProgressToken

	// Progress mode
	useDateProgress bool

	// Date-based progress
	totalSeconds    int64
	endTime         time.Time
	earliestMsgTime time.Time

	// Count-based progress
	messageCount int
	countLimit   int

	// Ticker for periodic notifications
	ticker  *time.Ticker
	done    chan struct{}
	lastMsg string
}

func newBackupProgress(
	ctx context.Context,
	srv *server.MCPServer,
	token mcp.ProgressToken,
	fromDate, toDate time.Time,
	countLimit int,
) *backupProgress {
	hasDateFilter := !fromDate.IsZero() || !toDate.IsZero()

	bp := &backupProgress{
		ctx:             ctx,
		srv:             srv,
		progressToken:   token,
		countLimit:      countLimit,
		useDateProgress: hasDateFilter && countLimit == 0,
		done:            make(chan struct{}),
	}

	if bp.useDateProgress {
		var startTime time.Time
		if !fromDate.IsZero() {
			startTime = fromDate
		} else {
			// If only "to" is specified, use Telegram launch date as start
			startTime = time.Date(2013, 8, 14, 0, 0, 0, 0, time.Local)
		}
		if !toDate.IsZero() {
			bp.endTime = toDate
		} else {
			bp.endTime = time.Now()
		}
		bp.totalSeconds = int64(bp.endTime.Sub(startTime).Seconds())
		if bp.totalSeconds < 1 {
			bp.totalSeconds = 1
		}
	}

	return bp
}

func (bp *backupProgress) Start() {
	bp.ticker = time.NewTicker(5 * time.Second)
	go func() {
		for {
			select {
			case <-bp.done:
				return
			case <-bp.ticker.C:
				if bp.lastMsg != "" {
					bp.Send(bp.lastMsg)
				}
			}
		}
	}()
}

func (bp *backupProgress) Stop() {
	if bp.ticker != nil {
		bp.ticker.Stop()
	}
	close(bp.done)
}

func (bp *backupProgress) SetMessage(msg string) {
	bp.lastMsg = msg
}

func (bp *backupProgress) SetMessageCount(count int) {
	bp.messageCount = count
}

func (bp *backupProgress) UpdateEarliestTime(t time.Time) {
	if bp.earliestMsgTime.IsZero() || t.Before(bp.earliestMsgTime) {
		bp.earliestMsgTime = t
	}
}

func (bp *backupProgress) getProgress() (progress float64, total int) {
	total = 100
	if bp.useDateProgress {
		if bp.earliestMsgTime.IsZero() {
			progress = 0
		} else {
			coveredSeconds := int64(bp.endTime.Sub(bp.earliestMsgTime).Seconds())
			if coveredSeconds < 0 {
				coveredSeconds = 0
			}
			progress = float64(coveredSeconds) / float64(bp.totalSeconds) * 100
			if progress > 100 {
				progress = 100
			}
		}
	} else {
		if bp.countLimit > 0 {
			progress = float64(bp.messageCount) / float64(bp.countLimit) * 100
			if progress > 100 {
				progress = 100
			}
		}
	}
	return
}

func (bp *backupProgress) Send(message string) {
	if bp.srv == nil {
		return
	}
	progress, total := bp.getProgress()
	payload := map[string]any{
		"progress": progress,
		"total":    total,
		"message":  message,
	}
	if bp.progressToken != nil {
		payload["progressToken"] = bp.progressToken
	}
	_ = bp.srv.SendNotificationToClient(bp.ctx, "notifications/progress", payload)
}

// Handle processes the BackupMessages tool request
func (h *MessageBackupHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatID := mcp.ParseInt64(request, "chat_id", 0)
	if chatID == 0 {
		return mcp.NewToolResultError("chat_id is required"), nil
	}

	targetPath := mcp.ParseString(request, "filepath", "")
	count := mcp.ParseInt(request, "count", 0)
	fromStr := mcp.ParseString(request, "from", "")
	toStr := mcp.ParseString(request, "to", "")

	// Parse dates
	fromDate, err := parseDate(fromStr)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	toDate, err := parseDate(toStr)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Default to 1000 messages if no filters specified
	if count == 0 && fromStr == "" && toStr == "" {
		count = 1000
	}

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

	// Validate a path against allowed directories
	if err := isPathAllowed(targetPath, h.allowedPaths); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Fetch messages with pagination
	const batchSize = 100
	var allMessages []*tg.Message
	userMap := make(map[int64]string)
	chatMap := make(map[int64]string)

	offsetID := 0
	offsetDate := 0
	if !toDate.IsZero() {
		// Add 1 day to toDate to include messages from that day
		offsetDate = int(toDate.Add(24 * time.Hour).Unix())
	}

	// Initialize progress tracker
	var progressToken mcp.ProgressToken
	if request.Params.Meta != nil {
		progressToken = request.Params.Meta.ProgressToken
	}
	progress := newBackupProgress(
		ctx,
		server.ServerFromContext(ctx),
		progressToken,
		fromDate, toDate,
		count,
	)
	progress.Start()
	defer progress.Stop()

	batchNum := 0

	limiter := ratelimit.New(1)

	for {
		batchNum++
		progress.SetMessage(fmt.Sprintf("Fetching messages (batch %d, %d messages so far)...", batchNum, len(allMessages)))

		historyRequest := &tg.MessagesGetHistoryRequest{
			Peer:       peer,
			Limit:      batchSize,
			OffsetID:   offsetID,
			OffsetDate: offsetDate,
		}

		limiter.Take()
		history, err := h.client.MessagesGetHistory(ctx, historyRequest)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to get messages: %v", err)), nil
		}

		var messages []tg.MessageClass
		var users []tg.UserClass
		var chats []tg.ChatClass

		switch hist := history.(type) {
		case *tg.MessagesMessages:
			messages = hist.Messages
			users = hist.Users
			chats = hist.Chats
		case *tg.MessagesMessagesSlice:
			messages = hist.Messages
			users = hist.Users
			chats = hist.Chats
		case *tg.MessagesChannelMessages:
			messages = hist.Messages
			users = hist.Users
			chats = hist.Chats
		default:
			return mcp.NewToolResultError("Unexpected response type"), nil
		}

		if len(messages) == 0 {
			progress.Send(fmt.Sprintf("No more messages in chat, collected %d messages", len(allMessages)))
			break
		}

		// Build user/chat maps
		for _, u := range users {
			if user, ok := u.(*tg.User); ok {
				userMap[user.ID] = tgclient.UserName(user)
			}
		}
		for _, c := range chats {
			switch chat := c.(type) {
			case *tg.Chat:
				chatMap[chat.ID] = chat.Title
			case *tg.Channel:
				chatMap[chat.ID] = chat.Title
			}
		}

		// Process messages
		reachedFromDate := false
		for _, msgClass := range messages {
			msg, ok := msgClass.(*tg.Message)
			if !ok {
				continue
			}

			msgTime := time.Unix(int64(msg.Date), 0)

			// Update the earliest message time for progress calculation
			progress.UpdateEarliestTime(msgTime)

			// Check if we've gone before fromDate
			if !fromDate.IsZero() && msgTime.Before(fromDate) {
				reachedFromDate = true
				break
			}

			allMessages = append(allMessages, msg)

			// Check count limit
			if count > 0 && len(allMessages) >= count {
				break
			}
		}

		// Update message count for progress
		progress.SetMessageCount(len(allMessages))

		// Stop conditions
		if reachedFromDate {
			progress.Send(fmt.Sprintf("Reached start date filter, collected %d messages", len(allMessages)))
			break
		}
		if count > 0 && len(allMessages) >= count {
			progress.Send(fmt.Sprintf("Reached count limit (%d messages)", count))
			break
		}
		if len(messages) < batchSize {
			progress.Send(fmt.Sprintf("Reached end of chat history, collected %d messages", len(allMessages)))
			break
		}

		// Get the last message ID for the next iteration
		if lastMsg, ok := messages[len(messages)-1].(*tg.Message); ok {
			offsetID = lastMsg.ID
			offsetDate = 0 // Reset after the first batch
		} else {
			progress.Send(fmt.Sprintf("Iteration stopped: no valid last message, collected %d messages", len(allMessages)))
			break
		}
	}

	// Format messages for backup
	var lines []string
	for _, msg := range allMessages {
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

		// Add a message
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

	// Write to a file
	content := strings.Join(lines, "\n")
	if err := os.WriteFile(targetPath, []byte(content), 0o600); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to write file: %v", err)), nil
	}

	// Get an absolute path for clear output
	absPath, _ := filepath.Abs(targetPath)

	result := fmt.Sprintf("Backup completed!\nMessages saved: %d\nFile: %s", len(allMessages), absPath)

	return mcp.NewToolResultText(result), nil
}
