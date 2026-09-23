package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tgdata"
	"github.com/tolmachov/mcp-telegram/internal/xdg"
)

// backupProgress state constants
const (
	progressStateCreated uint32 = iota
	progressStateRunning
	progressStateStopped
)

// telegramLaunchDate is the date when Telegram was launched (used as fallback for date range calculations).
var telegramLaunchDate = time.Date(2013, 8, 14, 0, 0, 0, 0, time.UTC)

// (Path-sandbox helpers — DefaultBackupDir, sanitizeFilename, isPathAllowed,
// resolveSymlinks — live in backup_path.go.)

// partialNote describes the partial messages a failed save was carrying, or
// is empty when the fetch completed.
func partialNote(count int, partialErr error) string {
	if partialErr == nil {
		return ""
	}
	return fmt.Sprintf(" for %d partial messages fetched before the error %q", count, partialErr.Error())
}

// MessageBackupHandler handles the BackupMessages tool.
type MessageBackupHandler struct {
	peers        *tgclient.Resolver
	provider     *messages.Provider
	allowedPaths []string
}

// NewMessageBackupHandler creates a new MessageBackupHandler.
func NewMessageBackupHandler(peers *tgclient.Resolver, provider *messages.Provider, allowedPaths []string) *MessageBackupHandler {
	return &MessageBackupHandler{
		peers:        peers,
		provider:     provider,
		allowedPaths: allowedPaths,
	}
}

// BackupMessagesInput is the input for the BackupMessages tool.
//
// The date/limit fields deliberately mirror the rest of the toolbox
// (from_date / to_date / limit, with an exclusive to_date) so callers don't
// have to learn a second convention. The only intentional divergence is that
// the date fields accept a couple of extra shorthand formats on top of RFC3339
// (see parseDate) — a strict superset, so anything valid elsewhere is valid
// here.
type BackupMessagesInput struct {
	ChatID   int64  `json:"chat_id" jsonschema:"The ID of the chat to backup messages from"`
	Filepath string `json:"filepath,omitempty" jsonschema:"Path to the file where messages will be saved (optional\\, auto-generated if not provided). If the file already exists\\, it will be overwritten."`
	Limit    int    `json:"limit,omitempty" jsonschema:"Maximum number of messages to backup (optional\\, default: 1000 if no date filters specified; recommended max: 10000). Larger backups may hit Telegram rate limits and take significantly longer."`
	FromDate string `json:"from_date,omitempty" jsonschema:"Start of the window (inclusive\\, optional). Accepts YYYY-MM-DD or YYYY-MM-DD HH:MM:SS (interpreted as UTC) or RFC3339 with an explicit offset."`
	ToDate   string `json:"to_date,omitempty" jsonschema:"End of the window (EXCLUSIVE\\, optional) - messages strictly before this instant. To include a whole day\\, pass the next day's date. Accepts YYYY-MM-DD or YYYY-MM-DD HH:MM:SS (interpreted as UTC) or RFC3339 with an explicit offset."`
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
	AddTool(s, &mcp.Tool{
		Name:        "BackupMessages",
		Description: "Backup messages from a chat to a text file. Messages are saved with timestamp, sender name, ID, and reply info. If filepath is not specified, generates automatic filename like 'ChatName-2024-01-15_10-30-00.txt' in default backup directory; otherwise overwrites the target file. Filters are optional — if none specified, backs up the last 1000 messages. Date window uses from_date (inclusive) and to_date (exclusive), matching GetMessages/SearchMessages; to include a whole day, pass the next day's date as to_date. For reading messages in-chat, use GetMessages instead.",
		Annotations: &mcp.ToolAnnotations{
			// Not idempotent: auto-named runs include time.Now() in the
			// filename so each call creates a distinct file. Callers that
			// provide an explicit filepath get file-overwrite semantics and
			// may treat that case as idempotent, but the tool as a whole
			// cannot advertise it.
			OpenWorldHint: new(true),
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
	count := in.Limit
	fromStr := in.FromDate
	toStr := in.ToDate

	// to_date is exclusive (strictly-less-than), matching every other tool's
	// date window — no inclusive-day fixup here.
	fromDate, toDate, errRes := parseDateWindow(fromStr, toStr)
	if errRes != nil {
		return errRes, nil, nil
	}

	// Default to 1000 messages if no filters specified.
	if count == 0 && fromStr == "" && toStr == "" {
		count = 1000
	}

	// Resolve the peer up front: it validates the chat before any file work
	// and carries the entity that names the backup file.
	op := fmt.Sprintf("back up chat %d", in.ChatID)
	peer, err := h.peers.Resolve(ctx, in.ChatID)
	if err != nil {
		return nil, nil, failed(op, err)
	}

	// Only operator-configured server paths are authority. MCP roots are
	// client-controlled metadata and must never widen a server-side write ACL.
	allowedPaths := h.allowedPaths

	// Generate filename if not provided.
	if targetPath == "" {
		if len(allowedPaths) == 0 {
			return errResult("no allowed paths configured for backup. Pass --allowed-paths / MCP_TELEGRAM_ALLOWED_PATHS."), nil, nil
		}
		chatName := tgdata.ChatInfoFromPeer(peer).Name
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
		requestProgressToken(req),
		fromDate, toDate,
		count,
	)
	if err := progress.Start(); err != nil {
		return nil, nil, failed(op, fmt.Errorf("starting progress: %w", err))
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
		return nil, nil, failed(op, err)
	}
	// Provider may return (nil, nil) which the guard above misses (no err to check).
	if result == nil {
		return nil, nil, failed(op, errors.New("provider returned no result"))
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
		return nil, nil, failed(op, fmt.Errorf("creating directory%s: %w", partialNote(len(result.Messages), partialErr), err))
	}

	// Replace the destination atomically so a crash cannot leave a truncated
	// backup that looks successful.
	if err := xdg.WriteFileAtomic(targetPath, []byte(content), 0o600, ".backup-*.tmp"); err != nil {
		return nil, nil, failed(op, fmt.Errorf("writing file%s: %w", partialNote(len(result.Messages), partialErr), err))
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
		return nil, nil, failedHint(op, partialErr, fmt.Sprintf(
			"The backup timed out; a partial file with %d messages was saved to %s. Retry with a narrower date window or smaller count.",
			len(result.Messages), absPath,
		))
	default:
		// Real mid-pagination failure (FLOOD_WAIT, transport error, etc.).
		// We persisted what we fetched so the user doesn't lose minutes of
		// work, but surface it as a tool error so the caller knows the
		// backup is incomplete and needs a retry anchored past the saved
		// file's last message.
		return nil, nil, failedHint(op, partialErr, fmt.Sprintf(
			"The backup stopped mid-stream; a partial file with %d messages was saved to %s. Retry with a narrower date window or resume from the last saved message.",
			len(result.Messages), absPath,
		))
	}
}
