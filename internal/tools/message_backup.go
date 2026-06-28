package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// backupProgress state constants
const (
	progressStateCreated uint32 = iota
	progressStateRunning
	progressStateStopped
)

// maxFilenameLength limits the base filename length to ensure compatibility
// across filesystems (most support 255 bytes, but we keep it conservative).
const maxFilenameLength = 100

// telegramLaunchDate is the date when Telegram was launched (used as fallback for date range calculations).
var telegramLaunchDate = time.Date(2013, 8, 14, 0, 0, 0, 0, time.UTC)

// DefaultBackupDir returns the default backup directory based on the OS.
// Returns an error when os.UserHomeDir fails (e.g. no HOME env var set,
// no passwd entry on Unix, or equivalent on other platforms), because in
// that case the resulting path would be relative to the process working
// directory rather than an absolute user directory.
func DefaultBackupDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory: %w", err)
	}

	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(homeDir, "Library", "Application Support", "mcp-telegram", "backups"), nil
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "mcp-telegram", "backups"), nil
		}
		return filepath.Join(homeDir, "AppData", "Roaming", "mcp-telegram", "backups"), nil
	default: // linux and others
		if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
			return filepath.Join(xdgData, "mcp-telegram", "backups"), nil
		}
		return filepath.Join(homeDir, ".local", "share", "mcp-telegram", "backups"), nil
	}
}

// sanitizeFilename removes or replaces characters that are invalid in filenames.
func sanitizeFilename(name string) string {
	invalid := []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|", "\n", "\r", "\t"}
	result := name
	for _, char := range invalid {
		result = strings.ReplaceAll(result, char, "_")
	}
	result = strings.Trim(result, " .")
	// Limit length (use runes to avoid splitting multi-byte UTF-8 characters).
	runes := []rune(result)
	if len(runes) > maxFilenameLength {
		result = string(runes[:maxFilenameLength])
	}
	if result == "" {
		result = "backup"
	}
	return result
}

// isPathAllowed checks if the given path is within one of the allowed
// directories. Both sides of the comparison are resolved through
// filepath.EvalSymlinks when the target (or parent) already exists on disk,
// so a symlink placed inside an allowed directory cannot be used to escape
// the sandbox. For paths that don't exist yet (the common case for a new
// backup file) we fall back to evaluating the parent directory — callers are
// expected to sanitise the filename separately via sanitizeFilename.
func isPathAllowed(targetPath string, allowedPaths []string) error {
	absTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return fmt.Errorf("resolving path: %w", err)
	}
	resolvedTarget, err := resolveSymlinks(absTarget)
	if err != nil {
		return fmt.Errorf("resolving target path %q: %w", targetPath, err)
	}

	// Track allowlist entries we had to skip because of unresolvable
	// ancestors; if every entry gets skipped we want the user to see the
	// real configuration error instead of a generic "not within allowed
	// directories" message.
	var skipReasons []string

	for _, allowed := range allowedPaths {
		absAllowed, err := filepath.Abs(allowed)
		if err != nil {
			skipReasons = append(skipReasons, fmt.Sprintf("%q: %v", allowed, err))
			continue
		}
		resolvedAllowed, err := resolveSymlinks(absAllowed)
		if err != nil {
			// If the allowlist entry itself can't be resolved (e.g. EACCES
			// on an ancestor), skip it rather than silently treating the
			// unresolved string as the sandbox root. A misconfigured
			// allowlist shouldn't widen the sandbox.
			skipReasons = append(skipReasons, fmt.Sprintf("%q: %v", allowed, err))
			continue
		}

		rel, err := filepath.Rel(resolvedAllowed, resolvedTarget)
		if err != nil {
			continue
		}

		if rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)) {
			return nil
		}
	}

	if len(allowedPaths) > 0 && len(skipReasons) == len(allowedPaths) {
		return fmt.Errorf("path %q is not within allowed directories: all %d allowlist entries are unresolvable (%s). Fix the configuration so the sandbox roots exist and are readable", targetPath, len(skipReasons), strings.Join(skipReasons, "; "))
	}

	return fmt.Errorf("path %q is not within allowed directories. Configure --allowed-paths or TELEGRAM_ALLOWED_PATHS", targetPath)
}

// resolveSymlinks returns the fully-resolved absolute path of p. When p
// itself does not exist (e.g. a backup filename that hasn't been written
// yet) it walks up to the first existing ancestor, resolves that, and
// re-attaches the unresolved tail. This keeps "target inside allowed-dir
// via symlink escape" attacks impossible while still allowing isPathAllowed
// to be called before the file exists.
//
// Non-ENOENT errors from EvalSymlinks (typically EACCES on an ancestor) are
// propagated as errors so the sandbox check can fail closed — silently
// reattaching the unresolved tail in that case would let an attacker who
// can place a symlink behind a permission-denied directory bypass the
// sandbox.
func resolveSymlinks(p string) (string, error) {
	p = filepath.Clean(p)
	resolved, err := filepath.EvalSymlinks(p)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// Path does not exist yet — walk up to the closest existing ancestor,
	// resolve that, and join the unresolved tail back on.
	parent := filepath.Dir(p)
	if parent == p {
		return p, nil
	}
	resolvedParent, err := resolveSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(p)), nil
}

// getChatName returns the display name of the chat and whether the name was
// successfully resolved. Falls back to "chat_%d" when the Telegram API call
// fails; resolved=false lets callers surface the fallback to users. Failures
// are logged at Warn so operators can diagnose unexpected fallback filenames.
func getChatName(ctx context.Context, raw *tg.Client, peer tg.InputPeerClass, chatID int64) (name string, resolved bool) {
	switch p := peer.(type) {
	case *tg.InputPeerUser:
		users, err := raw.UsersGetUsers(ctx, []tg.InputUserClass{
			&tg.InputUser{UserID: p.UserID, AccessHash: p.AccessHash},
		})
		if err != nil {
			slog.Warn("getChatName: user lookup failed; using fallback chat_<id>", "chat_id", chatID, "err", err)
		} else if len(users) > 0 {
			if user, ok := users[0].(*tg.User); ok {
				return tgclient.UserName(user), true
			}
			slog.Debug("getChatName: unexpected user type from API", "chat_id", chatID, "type", fmt.Sprintf("%T", users[0]))
		}
	case *tg.InputPeerChat:
		chats, err := raw.MessagesGetChats(ctx, []int64{p.ChatID})
		if err != nil {
			slog.Warn("getChatName: chat lookup failed; using fallback chat_<id>", "chat_id", chatID, "err", err)
		} else {
			// MessagesGetChats may return either MessagesChats or MessagesChatsSlice
			// (the latter when the server paginates large chat lists); both carry the
			// same Chats field.
			var chatList []tg.ChatClass
			switch r := chats.(type) {
			case *tg.MessagesChats:
				chatList = r.Chats
			case *tg.MessagesChatsSlice:
				chatList = r.Chats
			}
			if len(chatList) > 0 {
				if chat, ok := chatList[0].(*tg.Chat); ok {
					return chat.Title, true
				}
				slog.Debug("getChatName: unexpected chat type from API", "chat_id", chatID, "type", fmt.Sprintf("%T", chatList[0]))
			}
		}
	case *tg.InputPeerChannel:
		chats, err := raw.ChannelsGetChannels(ctx, []tg.InputChannelClass{
			&tg.InputChannel{ChannelID: p.ChannelID, AccessHash: p.AccessHash},
		})
		if err != nil {
			slog.Warn("getChatName: channel lookup failed; using fallback chat_<id>", "chat_id", chatID, "err", err)
		} else {
			// ChannelsGetChannels returns the same MessagesChats/MessagesChatsSlice
			// union as MessagesGetChats; handle both.
			var chatList []tg.ChatClass
			switch r := chats.(type) {
			case *tg.MessagesChats:
				chatList = r.Chats
			case *tg.MessagesChatsSlice:
				chatList = r.Chats
			}
			if len(chatList) > 0 {
				if channel, ok := chatList[0].(*tg.Channel); ok {
					return channel.Title, true
				}
				slog.Debug("getChatName: unexpected channel type from API", "chat_id", chatID, "type", fmt.Sprintf("%T", chatList[0]))
			}
		}
	}
	return fmt.Sprintf("chat_%d", chatID), false
}

// MessageBackupHandler handles the BackupMessages tool.
type MessageBackupHandler struct {
	client       *tg.Client
	provider     *messages.Provider
	allowedPaths []string
}

// NewMessageBackupHandler creates a new MessageBackupHandler.
func NewMessageBackupHandler(client *tg.Client, provider *messages.Provider, allowedPaths []string) *MessageBackupHandler {
	return &MessageBackupHandler{
		client:       client,
		provider:     provider,
		allowedPaths: allowedPaths,
	}
}

// BackupMessagesInput is the input for the BackupMessages tool.
type BackupMessagesInput struct {
	ChatID   int64  `json:"chat_id" jsonschema:"The ID of the chat to backup messages from"`
	Filepath string `json:"filepath,omitempty" jsonschema:"Path to the file where messages will be saved (optional\\, auto-generated if not provided). If the file already exists\\, it will be overwritten."`
	Count    int    `json:"count,omitempty" jsonschema:"Maximum number of messages to backup (optional\\, default: 1000 if no filters specified; recommended max: 10000). Larger backups may hit Telegram rate limits and take significantly longer."`
	From     string `json:"from,omitempty" jsonschema:"Start date - backup messages from this date (optional). Accepts YYYY-MM-DD or YYYY-MM-DD HH:MM:SS (interpreted as UTC) or RFC3339 with explicit offset for local windows."`
	To       string `json:"to,omitempty" jsonschema:"End date - backup messages until this date (optional). Accepts YYYY-MM-DD or YYYY-MM-DD HH:MM:SS (interpreted as UTC) or RFC3339 with explicit offset for local windows."`
}

// BackupMessagesResult is the typed output of BackupMessages. It accompanies
// the human-readable confirmation text so clients can read the saved path and
// message count as structured data instead of scraping the message. Partial is
// true when the file holds a partial backup (e.g. the run was cancelled
// mid-pagination).
type BackupMessagesResult struct {
	ChatID       int64  `json:"chat_id"`
	MessageCount int    `json:"message_count"`
	Filepath     string `json:"filepath"`
	Partial      bool   `json:"partial,omitempty"`
}

// Register adds the tool to the MCP server.
func (h *MessageBackupHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "BackupMessages",
		Description: "Backup messages from a chat to a text file. Messages are saved with timestamp, sender name, ID, and reply info. If filepath is not specified, generates automatic filename like 'ChatName-2024-01-15_10-30-00.txt' in default backup directory; otherwise overwrites the target file. All filter parameters are optional — if none specified, backs up last 1000 messages. For reading messages in-chat, use GetMessages instead.",
		Annotations: &mcp.ToolAnnotations{
			// Not idempotent: auto-named runs include time.Now() in the
			// filename so each call creates a distinct file. Callers that
			// provide an explicit filepath get file-overwrite semantics and
			// may treat that case as idempotent, but the tool as a whole
			// cannot advertise it.
			OpenWorldHint: ptrTrue(),
		},
	}, h.handle)
}

// parseDate accepts three formats:
//
//	YYYY-MM-DD                 → interpreted as midnight UTC
//	YYYY-MM-DD HH:MM:SS        → interpreted as UTC
//	RFC3339 (2006-01-02T15:04:05Z07:00) → with explicit zone
//
// UTC is the default (not time.Local) because distributed MCP agents —
// Claude Desktop on one machine, Claude Code CLI on another, or a remote
// container — can live in different timezones than the user issuing the
// prompt. Defaulting to Local would silently shift windows by hours
// depending on where the server happens to run. Callers that want a
// specific local window should pass RFC3339 with an explicit offset.
func parseDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.UTC); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid date format %q, expected YYYY-MM-DD, YYYY-MM-DD HH:MM:SS (UTC), or RFC3339 with explicit offset", s)
}

// normalizeInclusiveUpperDate keeps date-only "to" inputs inclusive for the
// whole day while preserving exact timestamps for RFC3339 / date-time inputs.
// Telegram's offset_date is strictly-less-than, so we advance to midnight of
// the next day (00:00:00) to include every message on the given YYYY-MM-DD.
func normalizeInclusiveUpperDate(raw string, parsed time.Time) time.Time {
	if parsed.IsZero() {
		return parsed
	}
	if _, err := time.Parse("2006-01-02", raw); err == nil {
		// parse success means raw is date-only (YYYY-MM-DD); advance by 24h
		// so the full day is included. The parsed time.Time is discarded —
		// we only need to know whether the layout matched, not the value.
		return parsed.Add(24 * time.Hour)
	}
	return parsed
}

// backupProgress handles progress tracking and notifications for message backup.
type backupProgress struct {
	ctx           context.Context
	session       *mcp.ServerSession
	progressToken any

	// Progress mode (immutable after creation).
	useDateProgress bool
	totalSeconds    int64
	endTime         time.Time
	countLimit      int

	// Mutable state protected by mutex.
	mu              sync.Mutex
	earliestMsgTime time.Time
	messageCount    int
	lastMsg         string

	// Lifecycle state protected by stateMu.
	stateMu sync.Mutex
	ticker  *time.Ticker
	done    chan struct{}
	state   uint32 // progressStateCreated -> progressStateRunning -> progressStateStopped
}

func newBackupProgress(
	ctx context.Context,
	session *mcp.ServerSession,
	token any,
	fromDate, toDate time.Time,
	countLimit int,
) *backupProgress {
	hasDateFilter := !fromDate.IsZero() || !toDate.IsZero()

	bp := &backupProgress{
		ctx:             ctx,
		session:         session,
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
			// If only "to" is specified, use Telegram launch date as start.
			startTime = telegramLaunchDate
		}
		if !toDate.IsZero() {
			bp.endTime = toDate
		} else {
			bp.endTime = time.Now()
		}
		bp.totalSeconds = max(int64(bp.endTime.Sub(startTime).Seconds()), 1)
	}

	return bp
}

func (bp *backupProgress) Start() error {
	bp.stateMu.Lock()
	defer bp.stateMu.Unlock()

	if bp.state != progressStateCreated {
		return fmt.Errorf("backupProgress already started")
	}
	bp.state = progressStateRunning
	bp.ticker = time.NewTicker(5 * time.Second)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("backupProgress goroutine panicked", "panic", r)
			}
		}()
		for {
			select {
			case <-bp.done:
				return
			case <-bp.ticker.C:
				bp.mu.Lock()
				msg := bp.lastMsg
				bp.mu.Unlock()
				if msg != "" {
					bp.Send(msg)
				}
			}
		}
	}()

	return nil
}

func (bp *backupProgress) Stop() error {
	bp.stateMu.Lock()
	defer bp.stateMu.Unlock()

	if bp.state != progressStateRunning {
		return fmt.Errorf("backupProgress is not running")
	}
	bp.state = progressStateStopped
	bp.ticker.Stop()
	close(bp.done)

	return nil
}

func (bp *backupProgress) SetMessage(msg string) {
	bp.mu.Lock()
	bp.lastMsg = msg
	bp.mu.Unlock()
}

func (bp *backupProgress) SetMessageCount(count int) {
	bp.mu.Lock()
	bp.messageCount = count
	bp.mu.Unlock()
}

func (bp *backupProgress) UpdateEarliestTime(t time.Time) {
	bp.mu.Lock()
	if bp.earliestMsgTime.IsZero() || t.Before(bp.earliestMsgTime) {
		bp.earliestMsgTime = t
	}
	bp.mu.Unlock()
}

func (bp *backupProgress) getProgress() (progress float64, total int) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	total = 100
	if bp.useDateProgress {
		if bp.earliestMsgTime.IsZero() {
			progress = 0
		} else {
			coveredSeconds := max(int64(bp.endTime.Sub(bp.earliestMsgTime).Seconds()), 0)
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
	progress, total := bp.getProgress()
	sendProgressWithToken(bp.ctx, bp.session, bp.progressToken, progress, float64(total), message)
}

func (h *MessageBackupHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in BackupMessagesInput) (*mcp.CallToolResult, *BackupMessagesResult, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}

	targetPath := in.Filepath
	count := in.Count
	fromStr := in.From
	toStr := in.To

	fromDate, err := parseDate(fromStr)
	if err != nil {
		return errResult(err.Error()), nil, nil
	}
	toDate, err := parseDate(toStr)
	if err != nil {
		return errResult(err.Error()), nil, nil
	}
	toDate = normalizeInclusiveUpperDate(toStr, toDate)
	if !fromDate.IsZero() && !toDate.IsZero() && fromDate.After(toDate) {
		return errResult(fmt.Sprintf("from (%s) is after to (%s); the date window is empty.", fromDate.Format(time.RFC3339), toDate.Format(time.RFC3339))), nil, nil
	}

	// Default to 1000 messages if no filters specified.
	if count == 0 && fromStr == "" && toStr == "" {
		count = 1000
	}

	// Resolve the peer for chat name lookup.
	peer, err := tgclient.ResolvePeer(ctx, h.client, in.ChatID)
	if err != nil {
		return errResolvePeer(in.ChatID, err), nil, nil
	}

	// Build the effective allow-list for this request: configured --allowed-paths
	// PLUS any directories the MCP client exposes via the roots capability. Roots
	// let an IDE-like host (Claude Code, Cursor) auto-grant the active workspace
	// without requiring users to pass paths via flags. Per MCP spec, roots use
	// file:// URIs and the client must declare the roots capability.
	allowedPaths := h.allowedPaths
	if rootPaths := rootsFromClient(ctx, req.Session); len(rootPaths) > 0 {
		allowedPaths = append(append([]string(nil), allowedPaths...), rootPaths...)
		mcpLog(ctx, req.Session, logLevelDebug, "BackupMessages", map[string]any{
			"merged_roots": rootPaths,
		})
	}

	// Generate filename if not provided.
	if targetPath == "" {
		if len(allowedPaths) == 0 {
			return errResult("no allowed paths configured for backup. Pass --allowed-paths / TELEGRAM_ALLOWED_PATHS, or grant the server access via your MCP client's workspace roots."), nil, nil
		}
		chatName, nameOK := getChatName(ctx, h.client, peer, in.ChatID)
		if !nameOK {
			mcpLog(ctx, req.Session, logLevelWarning, "BackupMessages", map[string]any{
				"action":  "chat_name_fallback",
				"chat_id": in.ChatID,
				"note":    "could not resolve chat display name; filename uses numeric ID",
			})
		}
		filename := fmt.Sprintf("%s-%s.txt", sanitizeFilename(chatName), time.Now().Format("2006-01-02_15-04-05"))
		targetPath = filepath.Join(allowedPaths[0], filename)
	}

	// Validate the path against allowed directories.
	if err := isPathAllowed(targetPath, allowedPaths); err != nil {
		return errResult(err.Error()), nil, nil
	}

	// Initialize progress tracker. Token may be nil if the client did not request progress;
	// in that case backupProgress.Send becomes a no-op via sendProgressWithToken.
	progress := newBackupProgress(
		ctx,
		req.Session,
		req.Params.GetProgressToken(),
		fromDate, toDate,
		count,
	)
	if err := progress.Start(); err != nil {
		return errResult(fmt.Sprintf("starting progress: %v", err)), nil, nil
	}
	defer func() {
		// Use slog directly: the request context is likely already cancelled
		// at defer time, so mcpLog(ctx, ...) would attempt a session write on
		// a dead context before falling back to slog — direct slog is simpler.
		// Stop() fails only when the state machine is in an unexpected state,
		// which indicates a bug in the progress lifecycle — use Error.
		if err := progress.Stop(); err != nil {
			slog.Error("BackupMessages: progress stop failed", "err", err)
		}
	}()

	mcpLog(ctx, req.Session, logLevelInfo, "BackupMessages", map[string]any{
		"chat_id":     in.ChatID,
		"target_path": targetPath,
		"count":       count,
		"from":        fromStr,
		"to":          toStr,
	})

	// Configure fetch options.
	opts := messages.FetchOptions{
		Limit:    100,
		MinDate:  fromDate,
		MaxDate:  toDate,
		MaxCount: count,
	}

	// Fetch messages using the provider with a progress callback.
	result, err := h.provider.FetchAll(ctx, in.ChatID, opts, func(batch int, collected int, earliestTime time.Time) {
		progress.SetMessage(fmt.Sprintf("Fetching messages (batch %d, %d messages so far)...", batch, collected))
		progress.SetMessageCount(collected)
		if !earliestTime.IsZero() {
			progress.UpdateEarliestTime(earliestTime)
		}
	})
	// FetchAll returns partial results (non-nil result alongside err) when
	// the context is cancelled or a mid-pagination batch fetch fails. When we
	// have something to save, persist it and report the partial state instead
	// of losing minutes of fetched history — that's the whole point of
	// long-running backup progress. Complete failures (result == nil) still
	// bubble up as tool errors.
	partialErr := err
	if err != nil && (result == nil || len(result.Messages) == 0) {
		mcpLog(ctx, req.Session, logLevelError, "BackupMessages", map[string]any{
			"chat_id": in.ChatID,
			"error":   err.Error(),
		})
		return errResult(fmt.Sprintf("Failed to get messages: %v", err)), nil, nil
	}
	// Provider may return (nil, nil) which the guard above misses (no err to check).
	if result == nil {
		return errResult("provider returned no result"), nil, nil
	}
	if partialErr != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "BackupMessages", map[string]any{
			"chat_id":          in.ChatID,
			"partial_messages": len(result.Messages),
			"error":            partialErr.Error(),
		})
	}

	progress.Send(fmt.Sprintf("Collected %d messages", len(result.Messages)))

	// Format messages for backup using the messages package.
	content := messages.FormatBatchForBackup(result.Messages)

	// Ensure parent directory exists.
	parentDir := filepath.Dir(targetPath)
	if err := os.MkdirAll(parentDir, 0o700); err != nil {
		if partialErr != nil {
			return errResult(fmt.Sprintf("Failed to create directory (%v) while trying to save %d partial messages from upstream error: %v", err, len(result.Messages), partialErr)), nil, nil
		}
		return errResult(fmt.Sprintf("Failed to create directory: %v", err)), nil, nil
	}

	// Write to a file.
	if err := os.WriteFile(targetPath, []byte(content), 0o600); err != nil {
		if partialErr != nil {
			return errResult(fmt.Sprintf("Failed to write file (%v) while trying to save %d partial messages from upstream error: %v", err, len(result.Messages), partialErr)), nil, nil
		}
		return errResult(fmt.Sprintf("Failed to write file: %v", err)), nil, nil
	}

	// Get an absolute path for clear output. On failure (e.g. Getwd returns
	// an error because the working directory was deleted) fall back to the
	// raw targetPath so the success message is still useful, and log the
	// anomaly.
	absPath := targetPath
	if resolved, err := filepath.Abs(targetPath); err == nil {
		absPath = resolved
	} else {
		mcpLog(ctx, req.Session, logLevelWarning, "BackupMessages", map[string]any{
			"action": "abs_path_failed",
			"path":   targetPath,
			"error":  err.Error(),
		})
	}
	switch {
	case partialErr == nil:
		return textResult(fmt.Sprintf("Backup completed!\nMessages saved: %d\nFile: %s", len(result.Messages), absPath)),
			&BackupMessagesResult{ChatID: in.ChatID, MessageCount: len(result.Messages), Filepath: absPath}, nil
	case errors.Is(partialErr, context.Canceled):
		// User-initiated cancel: not an error. Surface as success so the
		// caller can decide whether to resume, without the LLM treating the
		// partial file as a failure to retry blindly.
		return textResult(fmt.Sprintf(
				"Backup cancelled; partial file saved.\nMessages saved: %d\nFile: %s",
				len(result.Messages), absPath,
			)),
			&BackupMessagesResult{ChatID: in.ChatID, MessageCount: len(result.Messages), Filepath: absPath, Partial: true}, nil
	case errors.Is(partialErr, context.DeadlineExceeded):
		// Context deadline exceeded: surface as a tool error so the caller
		// knows the backup is incomplete and can retry with a narrower window.
		// The partial file is still useful, so we report it alongside the error.
		return errResult(fmt.Sprintf(
			"Backup timed out; partial file saved.\nMessages saved: %d\nFile: %s\nRetry with a narrower date window or smaller count.",
			len(result.Messages), absPath,
		)), nil, nil
	default:
		// Real mid-pagination failure (FLOOD_WAIT, transport error, etc.).
		// We persisted what we fetched so the user doesn't lose minutes of
		// work, but surface it as a tool error so the caller knows the
		// backup is incomplete and needs a retry anchored past the saved
		// file's last message.
		return errResult(fmt.Sprintf(
			"Backup failed mid-stream: %v\nPartial file saved with %d messages: %s\nRetry with a narrower date window or resume from the last saved message.",
			partialErr, len(result.Messages), absPath,
		)), nil, nil
	}
}
