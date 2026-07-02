package tools

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP logging level constants. The official SDK exposes LoggingLevel as a
// plain string type with no exported wire-value constants, so we define them
// here for readability at call sites.
const (
	logLevelDebug   mcp.LoggingLevel = "debug"
	logLevelInfo    mcp.LoggingLevel = "info"
	logLevelWarning mcp.LoggingLevel = "warning"
	logLevelError   mcp.LoggingLevel = "error"
)

// Handler is the interface every tool implements. Each handler registers
// itself with the server using the typed mcp.AddTool[In, Out] helper, which
// auto-validates input, generates the JSON schema, and populates structured
// output automatically.
type Handler interface {
	Register(s *mcp.Server)
}

// inputSchemaWithEnums infers the JSON schema for the input type In and overlays
// enum constraints onto the named properties. The SDK's reflection-based schema
// generation reads the `jsonschema` struct tag as a plain description only — it
// has no way to express an enum — so tools with a closed set of allowed string
// values (e.g. period, media_type) pass the result as mcp.Tool.InputSchema to
// turn the allowed set into a hard schema constraint the client can validate.
//
// It panics on a missing property or an inference error: both are programmer
// errors fixed at edit time, and Register has no error return.
func inputSchemaWithEnums[In any](enums map[string][]any) *jsonschema.Schema {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("inputSchemaWithEnums: inferring schema for %T: %v", *new(In), err))
	}
	for prop, vals := range enums {
		p, ok := schema.Properties[prop]
		if !ok {
			panic(fmt.Sprintf("inputSchemaWithEnums: property %q not found in schema for %T", prop, *new(In)))
		}
		p.Enum = vals
	}
	return schema
}

// RegisterTools registers all handlers with the MCP server.
func RegisterTools(s *mcp.Server, handlers []Handler) {
	for _, h := range handlers {
		h.Register(s)
	}
}

// parseDateFilter parses an RFC3339 date string for a filter field (e.g.
// "from_date"). Returns an errResult on parse failure; ok is false in that case.
func parseDateFilter(fieldName, value string) (t time.Time, errRes *mcp.CallToolResult, ok bool) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, errResult(fmt.Sprintf(
			"invalid %s %q: %v. Expected RFC3339 format, e.g. \"2026-04-09T00:00:00Z\".",
			fieldName, value, err,
		)), false
	}
	return parsed, nil, true
}

// ptrTrue returns a pointer to true. Used for *bool fields on
// mcp.ToolAnnotations (DestructiveHint, OpenWorldHint).
func ptrTrue() *bool {
	v := true
	return &v
}

// sendProgress sends a single progress notification for the given request.
// No-op when the request has no progress token. The token is set by the client
// in _meta.progressToken; the SDK exposes it via req.Params.GetProgressToken().
func sendProgress(ctx context.Context, req *mcp.CallToolRequest, progress, total float64, message string) {
	if req == nil || req.Session == nil {
		return
	}
	token := req.Params.GetProgressToken()
	if token == nil {
		return
	}
	if err := req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
		ProgressToken: token,
		Progress:      progress,
		Total:         total,
		Message:       message,
	}); err != nil {
		slog.Debug("progress notification failed", "err", err)
	}
}

// sendProgressWithToken sends a single progress notification using an explicit
// session + token captured at request time. Use this from long-lived progress
// trackers (e.g. backupProgress) that don't carry the request around.
func sendProgressWithToken(ctx context.Context, ss *mcp.ServerSession, token any, progress, total float64, message string) {
	if ss == nil || token == nil {
		return
	}
	if err := ss.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
		ProgressToken: token,
		Progress:      progress,
		Total:         total,
		Message:       message,
	}); err != nil {
		slog.Debug("progress notification failed", "err", err)
	}
}

// mcpLog sends a structured log message to the MCP client via
// notifications/message. Use this instead of stderr logging when the client
// should be able to surface the message (e.g. auth errors, long-running task
// diagnostics).
//
// When the MCP transport is unavailable (nil session), errors and warnings
// fall back to slog so the operator always has a record. Debug/info messages
// are silently dropped in that case — they are not operational signals.
// On delivery failure, errors and warnings are forwarded to slog.
func mcpLog(ctx context.Context, ss *mcp.ServerSession, level mcp.LoggingLevel, logger string, data any) {
	if ss == nil {
		switch level {
		case logLevelError:
			slog.Error("mcp log (no session)", "logger", logger, "data", data)
		case logLevelWarning:
			slog.Warn("mcp log (no session)", "logger", logger, "data", data)
		}
		return
	}
	if err := ss.Log(ctx, &mcp.LoggingMessageParams{
		Level:  level,
		Logger: logger,
		Data:   data,
	}); err != nil {
		switch level {
		case logLevelError:
			slog.Error("mcp log delivery failed", "logger", logger, "data", data, "err", err)
		case logLevelWarning:
			slog.Warn("mcp log delivery failed", "logger", logger, "data", data, "err", err)
		}
	}
}

// cryptoRandInt64 returns a cryptographically random int64 for use as
// Telegram's random_id message deduplication field.
func cryptoRandInt64() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never errors on supported platforms
	//nolint:gosec // G115: intentional full-width uint64→int64 reinterpretation for a random id; every bit pattern is a valid id.
	return int64(binary.LittleEndian.Uint64(b[:]))
}

// textResult constructs a CallToolResult with a single TextContent.
func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

// errResult constructs an error CallToolResult with a single TextContent.
// The model can read the error text and self-correct, so include actionable
// recovery hints in the message.
func errResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

// errChatIDRequired returns the canonical "chat_id is required" tool error
// with a recovery hint pointing to the discovery tools. Per MCP tool-design
// guidance, errors should turn dead ends into next steps.
func errChatIDRequired() *mcp.CallToolResult {
	return errResult("chat_id is required. Use SearchChats (by title) or ResolveUsername (by @handle) to find the numeric chat ID first.")
}

// errInvalidMessageID wraps a ParseMessageRef failure with an actionable
// recovery hint. Use this whenever a tool fails to parse an opaque message
// handle so the model understands the expected format and where to get
// valid handles from.
func errInvalidMessageID(s string, err error) *mcp.CallToolResult {
	return errResult(fmt.Sprintf(
		"invalid message_id %q: %v. Expected an opaque handle returned by GetMessages or SendMessage (e.g. \"42\" for a regular message, \"s:42\" for a scheduled one). Do not parse or construct handles manually.",
		s, err,
	))
}

// errCannotOnScheduled returns a targeted error when a tool is invoked with
// a scheduled message handle but the operation is not meaningful for
// scheduled messages (e.g. forwarding an unsent message, or using a scheduled
// message as a reply target). The verb should fit the pattern "cannot %s a
// scheduled message", e.g. "forward", "reply to".
func errCannotOnScheduled(verb string) *mcp.CallToolResult {
	return errResult(fmt.Sprintf(
		"cannot %s a scheduled message: it has not been sent yet and only exists in Telegram's schedule queue. Wait until it is delivered, or cancel it via DeleteMessage and create a new regular message.",
		verb,
	))
}

// floodWaitResult detects a Telegram FLOOD_WAIT — including the form the
// flood-wait middleware wraps when the wait exceeds its configured max
// (tgerr.AsFloodWait unwraps the chain) — and renders a deterministic,
// actionable error telling the caller exactly how long to wait. Returns
// (nil, false) when err is not a flood wait. The action verb fits "Telegram
// rate-limited this <action>", e.g. "join".
//
// FLOOD_WAIT here is an account-level limit (cumulative actions over a window,
// not request rate), so a local rate limiter cannot prevent it — the only
// remedy is to wait the reported duration and space the calls out, which the
// message states so the model stops retry-spamming.
func floodWaitResult(action string, err error) (*mcp.CallToolResult, bool) {
	msg, ok := floodWaitMessage(action, err)
	if !ok {
		return nil, false
	}
	return errResult(msg), true
}

// floodWaitMessage returns the deterministic retry-after guidance for a
// FLOOD_WAIT error, or ok=false when err is not a flood wait. Split out from
// floodWaitResult so batch handlers (e.g. MarkAsRead) can embed the same text in
// an aggregated result instead of a standalone error.
func floodWaitMessage(action string, err error) (string, bool) {
	d, ok := tgerr.AsFloodWait(err)
	if !ok {
		return "", false
	}
	d = d.Round(time.Second)
	return fmt.Sprintf(
		"Telegram rate-limited this %s: wait %s (%d seconds) before retrying. This is an account-level flood limit (cumulative actions, not request rate), so spacing out %s calls is the only way to avoid it — do not retry immediately.",
		action, d, int(d/time.Second), action,
	), true
}

// telegramErrResult renders a failed Telegram API call as a tool error,
// preferring the deterministic FLOOD_WAIT message (floodWaitResult) when the
// failure is a rate limit and falling back to a plain "Failed to <action>"
// otherwise. Single-call write tools (SendMessage, ForwardMessage, SetReaction,
// EditMessage, …) route their API errors through this so a FLOOD_WAIT never
// reaches the model as a bare "rpc error 420" that invites an immediate retry.
// Batch tools like MarkAsRead instead call floodWaitMessage directly so they can
// stop the batch early and still emit a structured result. The action verb fits
// both "Telegram rate-limited this <action>" and "Failed to <action>", e.g.
// "send message".
func telegramErrResult(action string, err error) *mcp.CallToolResult {
	if res, ok := floodWaitResult(action, err); ok {
		return res
	}
	return errResult(fmt.Sprintf("Failed to %s: %v", action, err))
}

// errResolvePeer wraps a peer-resolution failure with an actionable hint.
// Use this whenever tgclient.ResolvePeer fails so the model knows how to
// recover (e.g. switch to SearchChats / GetChats / ResolveUsername).
func errResolvePeer(chatID int64, err error) *mcp.CallToolResult {
	return errResult(fmt.Sprintf(
		"Failed to resolve chat %d: %v. The chat may not exist, you may not have access, or the ID may be wrong. Use SearchChats or GetChats to verify, or ResolveUsername if you only have a @handle.",
		chatID, err,
	))
}

// confirmDestructive asks the user to confirm a destructive action via MCP
// elicitation. Returns (true, nil) when the user confirms, or when the client
// has a session but doesn't advertise elicitation capability (graceful
// fallback — Claude is already instructed via Server.Instructions to confirm
// verbally before proceeding). Returns (false, error) when there is no MCP
// session at all (fail-closed: cannot present a confirmation UI).
//
// Per the official SDK pattern, we check the elicitation capability via
// InitializeParams BEFORE calling Elicit, mirroring how the SDK itself
// detects unsupported clients internally. This is cleaner than catching an
// error after the call and avoids the previous SDK's reliance on a
// not-supported sentinel error that doesn't exist in the official SDK.
//
// The flat-form schema below complies with the elicitation spec's "primitives
// only, no nesting" constraint.
func confirmDestructive(ctx context.Context, req *mcp.CallToolRequest, message string) (bool, error) {
	if req == nil || req.Session == nil {
		// Fail closed: no session means we cannot present a confirmation UI.
		// Return an error so callers can surface a clear diagnostic instead of
		// the misleading "Cancelled by user" message.
		return false, fmt.Errorf("no MCP session available to confirm destructive action")
	}
	iparams := req.Session.InitializeParams()
	if iparams == nil || iparams.Capabilities == nil || iparams.Capabilities.Elicitation == nil {
		// Client doesn't support elicitation — proceed without UI confirmation.
		// This also fires in non-interactive contexts (scripts, test harnesses).
		// Log to both MCP session and slog so operators see it regardless of
		// whether the client subscribes to log notifications.
		slog.Info("confirmDestructive: client does not support elicitation; proceeding without confirmation")
		mcpLog(ctx, req.Session, logLevelWarning, "elicitation", map[string]any{
			"action":   "fallback_no_capability",
			"behavior": "proceeding without UI confirm; client does not support elicitation",
		})
		return true, nil
	}
	result, err := req.Session.Elicit(ctx, &mcp.ElicitParams{
		Mode:    "form",
		Message: message,
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"confirm": map[string]any{
					"type":        "boolean",
					"title":       "Confirm",
					"description": "Tick to proceed; leave unchecked or cancel to abort.",
				},
			},
			"required": []string{"confirm"},
		},
	})
	if err != nil {
		// Capability said yes but the call failed — log and decline rather
		// than gracefully proceeding (the client *can* support elicitation,
		// so a failure here is a real error worth surfacing).
		mcpLog(ctx, req.Session, logLevelError, "elicitation", map[string]any{
			"action": "elicit_failed",
			"error":  err.Error(),
		})
		return false, fmt.Errorf("eliciting confirmation: %w", err)
	}
	if result.Action != "accept" {
		return false, nil
	}
	// The SDK validates the response against RequestedSchema before returning, so
	// result.Content["confirm"] should always be a bool when Action == "accept".
	// The assertion can still fail if the SDK contract changes or a custom client
	// sends a non-conformant response; the Warn makes that visible and the
	// safe-default false ensures we decline rather than proceed.
	confirmed, ok := result.Content["confirm"].(bool)
	if !ok {
		slog.Warn("confirmDestructive: elicitation response missing bool confirm field; declining")
	}
	return confirmed, nil
}

// rootsFromClient queries the MCP client for its workspace roots and returns
// the local filesystem paths from any file:// URIs. Non-file roots are skipped;
// malformed file:// URIs are logged at Debug level. Returns an empty slice when:
//   - the client did not declare the roots capability
//   - the client returned an error
//   - the roots list is empty or contains no file:// URIs
//
// Per MCP spec, this is best-effort: a server should not fail if the client
// doesn't expose roots — it should fall back to its own configured allow-list.
func rootsFromClient(ctx context.Context, ss *mcp.ServerSession) []string {
	if ss == nil {
		return nil
	}
	iparams := ss.InitializeParams()
	if iparams == nil || iparams.Capabilities == nil {
		return nil
	}
	// Roots is a value type on ClientCapabilities (not a pointer), so we can't
	// nil-check it directly. Instead, attempt the call and ignore errors —
	// hosts that don't support roots will return an error or empty list.
	result, err := ss.ListRoots(ctx, &mcp.ListRootsParams{})
	if err != nil {
		slog.Debug("rootsFromClient: ListRoots failed", "err", err)
		return nil
	}
	if result == nil {
		return nil
	}
	paths := make([]string, 0, len(result.Roots))
	for _, root := range result.Roots {
		path, err := fileURIToPath(root.URI)
		if err != nil {
			// Non-file:// roots (http://, etc.) are legitimately skipped per
			// MCP spec. Log only when the scheme is file:// to distinguish a
			// malformed path grant from a deliberate non-file root.
			if len(root.URI) >= 7 && root.URI[:7] == "file://" {
				slog.Debug("rootsFromClient: skipping malformed file URI", "uri", root.URI, "err", err)
			}
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

// fileURIToPath converts a file:// URI into a local filesystem path.
// It handles percent-decoding and Windows drive-letter forms like
// file:///C:/Users/alice/project, which url.Parse leaves as /C:/...
func fileURIToPath(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parsing file uri %q: %w", raw, err)
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("unsupported uri scheme %q", u.Scheme)
	}
	p, err := url.PathUnescape(u.Path)
	if err != nil {
		return "", fmt.Errorf("unescaping file uri path %q: %w", u.Path, err)
	}
	// Windows file URIs carry drive letters in the path with a leading slash.
	// Preserve UNC hosts and strip the synthetic leading slash only for drive
	// letters. filepath.FromSlash converts separators appropriately on Windows.
	if runtime.GOOS == "windows" {
		if u.Host != "" {
			p = `\\` + u.Host + filepath.FromSlash(p)
		} else if len(p) >= 3 && p[0] == '/' && p[2] == ':' && ((p[1] >= 'A' && p[1] <= 'Z') || (p[1] >= 'a' && p[1] <= 'z')) {
			p = p[1:]
		}
		return filepath.Clean(filepath.FromSlash(p)), nil
	}
	if u.Host != "" {
		p = "//" + u.Host + p
	}
	// Keep POSIX paths slash-based so file:// URIs from clients on Unix map
	// exactly to local paths on the same machine.
	return filepath.Clean(strings.ReplaceAll(p, `\`, `/`)), nil
}

// clampLimit returns limit clamped to [1, maxLimit], or defaultVal when limit is non-positive.
func clampLimit(limit, defaultVal, maxLimit int) int {
	if limit <= 0 {
		return defaultVal
	}
	if limit > maxLimit {
		return maxLimit
	}
	return limit
}
