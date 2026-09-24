package tools

import (
	"context"
	"errors"
	"fmt"
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

// telegramLaunchDate is the date when Telegram was launched (used as fallback
// for date range calculations).
var telegramLaunchDate = time.Date(2013, 8, 14, 0, 0, 0, 0, time.UTC)

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

// backupProgressInterval spaces a backup's progress notifications.
const backupProgressInterval = 5 * time.Second

// backupProgress reports a running backup's progress to the client of its
// call every backupProgressInterval, as the percentage of the date window
// covered when dates alone bound the backup, or of the count limit.
type backupProgress struct {
	ctx context.Context
	req *mcp.CallToolRequest
	// windowEnd and windowSeconds describe the date window when dates alone
	// bound the backup (windowSeconds > 0); countLimit is the count limit,
	// or 0 for none.
	windowEnd     time.Time
	windowSeconds int64
	countLimit    int

	mu        sync.Mutex
	message   string
	collected int
	earliest  time.Time
}

// startBackupProgress starts reporting progress for the call req; stop ends
// the reports and returns once none is in flight.
func startBackupProgress(ctx context.Context, req *mcp.CallToolRequest, fromDate, toDate time.Time, countLimit int) (bp *backupProgress, stop func()) {
	bp = &backupProgress{ctx: ctx, req: req, countLimit: countLimit}
	if countLimit == 0 && (!fromDate.IsZero() || !toDate.IsZero()) {
		start, end := fromDate, toDate
		if start.IsZero() {
			start = telegramLaunchDate
		}
		if end.IsZero() {
			end = time.Now()
		}
		bp.windowEnd = end
		bp.windowSeconds = max(int64(end.Sub(start).Seconds()), 1)
	}
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(backupProgressInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				bp.mu.Lock()
				message := bp.message
				bp.mu.Unlock()
				if message != "" {
					bp.send(message)
				}
			}
		}
	}()
	return bp, func() {
		close(done)
		<-finished
	}
}

// update records the fetch's progress after batch: the messages collected so
// far and the earliest date the batch held (zero when it held none).
func (bp *backupProgress) update(batch, collected int, earliest time.Time) {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	bp.message = fmt.Sprintf("Fetching messages (batch %d, %d messages so far)...", batch, collected)
	bp.collected = collected
	if !earliest.IsZero() && (bp.earliest.IsZero() || earliest.Before(bp.earliest)) {
		bp.earliest = earliest
	}
}

// percent returns how far the backup has got, from 0 to 100.
func (bp *backupProgress) percent() float64 {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	switch {
	case bp.windowSeconds > 0 && !bp.earliest.IsZero():
		covered := max(int64(bp.windowEnd.Sub(bp.earliest).Seconds()), 0)
		return min(float64(covered)/float64(bp.windowSeconds)*100, 100)
	case bp.windowSeconds == 0 && bp.countLimit > 0:
		return min(float64(bp.collected)/float64(bp.countLimit)*100, 100)
	default:
		return 0
	}
}

// send reports the current progress with message.
func (bp *backupProgress) send(message string) {
	sendProgress(bp.ctx, bp.req, bp.percent(), 100, message)
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

	progress, stopProgress := startBackupProgress(ctx, req, fromDate, toDate, count)
	defer stopProgress()

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
	result, err := h.provider.FetchAll(ctx, in.ChatID, opts, progress.update)
	// FetchAll returns partial results (non-nil result alongside err) when
	// the context is cancelled or a mid-pagination batch fetch fails. When we
	// have something to save, persist it and report the partial state instead
	// of losing minutes of fetched history — that's the whole point of
	// long-running backup progress. Complete failures (result == nil) still
	// bubble up as tool errors; without an error FetchAll always returns a
	// result.
	partialErr := err
	if err != nil && (result == nil || len(result.Messages) == 0) {
		return nil, nil, failed(op, err)
	}
	if partialErr != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "BackupMessages", map[string]any{
			"chat_id":          in.ChatID,
			"partial_messages": len(result.Messages),
			"error":            partialErr.Error(),
		})
	}

	progress.send(fmt.Sprintf("Collected %d messages", len(result.Messages)))

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
	case errors.Is(partialErr, context.Canceled) && ctx.Err() != nil:
		// User-initiated cancel: not an error. Surface as success so the
		// caller can decide whether to resume, without the LLM treating the
		// partial file as a failure to retry blindly. Only the call's own
		// context says the caller cancelled: a fetch cancelled while the call
		// still runs was cut short by the Telegram client stopping under it,
		// which is a failure.
		return textResult(fmt.Sprintf(
				"Backup cancelled; partial file saved.\nMessages saved: %d\nFile: %s",
				len(result.Messages), absPath,
			)),
			&BackupMessagesResult{ChatID: in.ChatID, MessageCount: len(result.Messages), Filepath: absPath, Partial: true}, nil
	default:
		return nil, nil, partialBackupFailure(op, partialErr, len(result.Messages), absPath)
	}
}

// partialBackupFailure reports a fetch that failed mid-pagination (a timeout,
// FLOOD_WAIT, a transport error, etc.) after count messages were saved to
// path. We persisted what we fetched so the user doesn't lose minutes of work,
// but surface it as a tool error so the caller knows the backup is incomplete
// and needs a retry anchored past the saved file's last message.
//
// The note reaches the model whatever the failure, the hint only when the
// failure is not systemic (see failureText). A timeout is systemic, yet a
// smaller request can avoid it, so its retry advice goes in the note — unless
// it struck while the call waited out a wait Telegram told it to take: then
// the wait is the cause, and the failure says how long it still asks for.
func partialBackupFailure(op string, err error, count int, path string) error {
	saved := fmt.Sprintf("a partial file with %d messages was saved to %s.", count, path)
	if _, told := tgclient.RetryAfter(err); errors.Is(err, context.DeadlineExceeded) && !told {
		return failed(op, withNote(err, "The backup timed out; "+saved+
			" Retry with a narrower date window or a smaller limit, or resume from the last saved message."))
	}
	return failedHint(op, withNote(err, "The backup stopped mid-stream; "+saved),
		"Retry with a narrower date window or resume from the last saved message.")
}
