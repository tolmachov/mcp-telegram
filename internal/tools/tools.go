package tools

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
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
func parseDate(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid date format %q, expected YYYY-MM-DD, YYYY-MM-DD HH:MM:SS (UTC), or RFC3339 with explicit offset", value)
}

func parseDateWindow(from, to string) (time.Time, time.Time, *mcp.CallToolResult) {
	minDate, err := parseDate(from)
	if err != nil {
		return time.Time{}, time.Time{}, errResult(fmt.Sprintf("invalid from_date: %v", err))
	}
	maxDate, err := parseDate(to)
	if err != nil {
		return time.Time{}, time.Time{}, errResult(fmt.Sprintf("invalid to_date: %v", err))
	}
	if !minDate.IsZero() && !maxDate.IsZero() && !minDate.Before(maxDate) {
		return time.Time{}, time.Time{}, errResult(fmt.Sprintf("from_date (%s) is not before to_date (%s); the date window is empty.", minDate.Format(time.RFC3339), maxDate.Format(time.RFC3339)))
	}
	return minDate, maxDate, nil
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
	token := requestProgressToken(req)
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

func requestProgressToken(req *mcp.CallToolRequest) any {
	if req == nil || req.Params == nil {
		return nil
	}
	return req.Params.GetProgressToken()
}

func requestSession(req *mcp.CallToolRequest) *mcp.ServerSession {
	if req == nil {
		return nil
	}
	return req.Session
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

// AddTool registers a typed tool so that a non-success outcome can never reach
// the client as a zero-valued output. For a pointer output type the SDK fills
// StructuredContent from the zero value whenever the handler returns a nil
// output, and hosts that render structured content then show an empty
// {"status":""} object instead of the error text or the real outcome. So:
//   - an IsError result is turned into a Go error, which the SDK sends as
//     IsError + text with no structured content;
//   - a non-error result without a typed output is a handler bug and is
//     reported as an error rather than an empty success.
//
// Every tool with a typed output must be registered through this instead of
// mcp.AddTool.
func AddTool[In, Out any](s *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, *Out]) {
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, *Out, error) {
		res, out, err := h(ctx, req, in)
		if err != nil {
			return nil, nil, err
		}
		if res != nil && res.IsError {
			return nil, nil, errors.New(toolResultText(res))
		}
		if out == nil {
			return nil, nil, fmt.Errorf("%s returned no result (server bug): the outcome of this call is unknown", t.Name)
		}
		return res, out, nil
	})
}

// toolResultText concatenates the text blocks of a tool result.
func toolResultText(r *mcp.CallToolResult) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
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
		"cannot %s a scheduled message: it has not been sent yet and only exists in Telegram's schedule queue. Wait until it is delivered, or cancel it via DeleteMessages and create a new regular message.",
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

// requireExplicitConfirmation is the sole authority gate for irreversible
// tool calls. MCP UI capabilities and model instructions are advisory only.
func requireExplicitConfirmation(confirm bool, action string) *mcp.CallToolResult {
	if confirm {
		return nil
	}
	return errResult(fmt.Sprintf("explicit confirmation required: set confirm=true to %s after the user approves the action", action))
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
