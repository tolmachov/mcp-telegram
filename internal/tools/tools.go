package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/presentation"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MCP logging level constants. The official SDK exposes LoggingLevel as a
// plain string type with no exported wire-value constants, so we define them
// here for readability at call sites.
const (
	logLevelInfo    mcp.LoggingLevel = "info"
	logLevelWarning mcp.LoggingLevel = "warning"
	logLevelError   mcp.LoggingLevel = "error"
)

// Handler is the interface every tool implements. Each handler registers
// itself with the server through AddTool (or AddContentTool for a tool with no
// typed output), which wrap the SDK's typed mcp.AddTool: input is validated,
// the JSON schema generated and structured output populated automatically.
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
func inputSchemaWithEnums[In any](enums map[string][]string) *jsonschema.Schema {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("inputSchemaWithEnums: inferring schema for %T: %v", *new(In), err))
	}
	for prop, vals := range enums {
		p, ok := schema.Properties[prop]
		if !ok {
			panic(fmt.Sprintf("inputSchemaWithEnums: property %q not found in schema for %T", prop, *new(In)))
		}
		p.Enum = make([]any, len(vals))
		for i, v := range vals {
			p.Enum[i] = v
		}
	}
	return schema
}

// RegisterTools registers all handlers with the MCP server.
func RegisterTools(s *mcp.Server, handlers []Handler) {
	for _, h := range handlers {
		h.Register(s)
	}
}

// parseDate parses a date filter value (e.g. "from_date"): RFC3339, or
// "YYYY-MM-DD[ HH:MM:SS]" read as UTC. An empty value yields the zero time,
// meaning no bound.
//
// UTC is the default (not time.Local) because distributed MCP agents —
// Claude Desktop on one machine, Claude Code CLI on another, or a remote
// container — can live in different timezones than the user issuing the
// prompt. Defaulting to Local would silently shift windows by hours
// depending on where the server happens to run. Callers that want a
// specific local window should pass RFC3339 with an explicit offset.
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

// sendProgress sends a single progress notification for the given request.
// No-op when the request has no progress token. The token is set by the client
// in _meta.progressToken; the SDK exposes it via req.Params.GetProgressToken().
func sendProgress(ctx context.Context, req *mcp.CallToolRequest, progress, total float64, message string) {
	if req.Session == nil || req.Params == nil {
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
	msg := "mcp log (no session)"
	var err error
	if ss != nil {
		if err = ss.Log(ctx, &mcp.LoggingMessageParams{Level: level, Logger: logger, Data: data}); err == nil {
			return
		}
		msg = "mcp log delivery failed"
	}
	var slogLevel slog.Level
	switch level {
	case logLevelError:
		slogLevel = slog.LevelError
	case logLevelWarning:
		slogLevel = slog.LevelWarn
	default:
		return
	}
	attrs := []any{"logger", logger, "data", data}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	slog.Log(ctx, slogLevel, msg, attrs...)
}

// AddTool registers a typed tool so that a non-success outcome can never reach
// the client as a zero-valued output. For a pointer output type the SDK fills
// StructuredContent from the zero value whenever the handler returns a nil
// output, and hosts that render structured content then show an empty
// {"status":""} object instead of the error text or the real outcome. So:
//   - a handler error is rendered and logged by toolFailure;
//   - an IsError result (input validation) is turned into a Go error, which
//     the SDK sends as IsError + text with no structured content;
//   - a non-error result without a typed output is a handler bug and is
//     reported as an error rather than an empty success.
//
// The handler runs with a flood-wait budget of its own
// (tgclient.WithWaitBudget), so the Telegram calls of one tool call wait no
// more than the configured maximum in all.
//
// Every tool with a typed output must be registered through this instead of
// mcp.AddTool.
func AddTool[In, Out any](s *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, *Out]) {
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, *Out, error) {
		res, out, err := h(tgclient.WithWaitBudget(ctx), req, in)
		if err != nil {
			return nil, nil, toolFailure(ctx, req, t.Name, err)
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

// AddContentTool registers a tool with no typed output (e.g. GetMedia, which
// returns image content), routing its handler errors through toolFailure and
// giving it a flood-wait budget like AddTool does.
func AddContentTool[In any](s *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, any]) {
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		res, out, err := h(tgclient.WithWaitBudget(ctx), req, in)
		if err != nil {
			return nil, nil, toolFailure(ctx, req, t.Name, err)
		}
		return res, out, nil
	})
}

// failure is a tool call that failed past input validation — a Telegram RPC
// or another runtime error. Handlers return it as their Go error and
// toolFailure renders it as "Failed to <op>: <err>" plus the note and the
// hint. A failure without an op only carries a note or hint for an outer
// failure to render.
//
// A note states an outcome the model must know whatever went wrong, e.g. the
// partial file a backup saved; a hint suggests how to fix the request, which
// cannot cure a systemic failure (tgclient.IsSystemic), so it is dropped
// there.
type failure struct {
	op   string
	note string
	hint string
	err  error
}

func (f *failure) Error() string {
	if f.op == "" {
		return f.err.Error()
	}
	return f.op + ": " + f.err.Error()
}

func (f *failure) Unwrap() error { return f.err }

// errNoCause stands in for the error of a failure built without one — a
// handler bug — so rendering it still says something true instead of
// panicking on a nil error.
var errNoCause = errors.New("the server recorded no cause for this failure (server bug)")

// newFailure is the one constructor of failure.
func newFailure(op, note, hint string, err error) error {
	if err == nil {
		err = errNoCause
	}
	return &failure{op: op, note: note, hint: hint, err: err}
}

// failed reports that op — a phrase fitting "Failed to <op>", e.g. "send
// message" — failed with err.
func failed(op string, err error) error { return newFailure(op, "", "", err) }

// failedHint is failed with a recovery hint the model can act on.
func failedHint(op string, err error, hint string) error { return newFailure(op, "", hint, err) }

// withHint attaches a recovery hint to err for the failure that wraps it.
func withHint(err error, hint string) error { return newFailure("", "", hint, err) }

// withNote attaches an outcome note to err for the failure that wraps it.
func withNote(err error, note string) error { return newFailure("", note, "", err) }

// peerHint follows a failure about the one chat a call named (tgclient.IsPeerSpecific).
const peerHint = "The chat may not exist, you may not have access, or the ID may be wrong. Use SearchChats or GetChats to verify, or ResolveUsername if you only have a @handle."

// toolFailure is the single place a handler's Go error becomes the tool error
// the model reads: it classifies err through tgclient, renders it, and logs
// it under the tool's name.
func toolFailure(ctx context.Context, req *mcp.CallToolRequest, tool string, err error) error {
	level := logLevelError
	if _, ok := tgclient.RetryAfter(err); ok {
		level = logLevelWarning
	}
	mcpLog(ctx, req.Session, level, tool, map[string]any{"error": err.Error()})
	return errors.New(failureText(tool, err))
}

// failureText renders a handler error as "Failed to <op>: <what happened>",
// followed by the failure's note and then the hint describe picks.
func failureText(tool string, err error) string {
	// The outermost op names what failed; the outermost note and hint win.
	op, note, hint, cause := "run "+tool, "", "", err
	named := false
	for e := err; ; {
		var f *failure
		if !errors.As(e, &f) {
			break
		}
		if !named && f.op != "" {
			op, cause, named = f.op, f.err, true
		}
		if note == "" {
			note = f.note
		}
		if hint == "" {
			hint = f.hint
		}
		e = f.err
	}
	what, hint := describe(tool, cause, hint)
	text := fmt.Sprintf("Failed to %s: %s", op, what)
	for _, s := range []string{note, hint} {
		if s != "" {
			text += " " + s
		}
	}
	return text
}

// describe is the one rendering of an error a tool reports, whether as its
// failure (failureText) or as the warning of a batch the error cut short: it
// returns what happened and the hint to follow it, given hint, the one the
// failure carries. A wait Telegram told the call to take gets its fixed
// guidance as what happened, and no hint. Any other condition no change to
// the request can cure (tgclient.IsBeyondRequest) gets no hint either: a dead
// session or a stopped client is explained by the server, not here, because
// how to recover depends on the transport — the server appends its
// explanation to the call the client stopped under and answers every later
// one with it. Anything else shows the error itself, followed by hint or,
// lacking one, the peer hint when the error is about the chat the call named.
func describe(tool string, cause error, hint string) (what, next string) {
	switch flood, isWait := floodWaitMessage(tool, cause); {
	case isWait:
		return flood, ""
	case tgclient.IsBeyondRequest(cause):
		return sentence(cause), ""
	case hint == "" && tgclient.IsPeerSpecific(cause):
		return sentence(cause), peerHint
	default:
		return sentence(cause), hint
	}
}

// sentence renders err as a sentence ending in a full stop, so a hint can
// follow it.
func sentence(err error) string {
	return strings.TrimSuffix(err.Error(), ".") + "."
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

// errInvalidMessageID wraps a ParseMessageRef failure of the named input field
// with an actionable recovery hint. Use this whenever a tool fails to parse an
// opaque message handle so the model understands the expected format and where
// to get valid handles from.
func errInvalidMessageID(field, s string, err error) *mcp.CallToolResult {
	return errResult(fmt.Sprintf(
		"invalid %s %q: %v. Expected an opaque handle returned by GetMessages or SendMessage (e.g. \"42\" for a regular message, \"s:42\" for a scheduled one). Do not parse or construct handles manually.",
		field, s, err,
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

// parseRegularRef parses the opaque message handle s passed in the named input
// field and returns its message ID, rejecting scheduled handles: the operation,
// described by verb as in errCannotOnScheduled, needs a message that was sent.
func parseRegularRef(field, s, verb string) (int, *mcp.CallToolResult) {
	ref, err := presentation.ParseMessageRef(s)
	if err != nil {
		return 0, errInvalidMessageID(field, s, err)
	}
	if ref.Scheduled {
		return 0, errCannotOnScheduled(verb)
	}
	return ref.ID, nil
}

// parseFutureSchedule parses a schedule_at value, which must be an RFC3339
// timestamp in the future.
func parseFutureSchedule(scheduleAt string) (time.Time, *mcp.CallToolResult) {
	t, err := time.Parse(time.RFC3339, scheduleAt)
	if err != nil {
		return time.Time{}, errResult(fmt.Sprintf("invalid schedule_at %q: %v. Expected RFC3339 format like \"2026-04-10T15:30:00Z\".", scheduleAt, err))
	}
	if !t.After(time.Now()) {
		return time.Time{}, errResult("schedule_at must be in the future")
	}
	return t, nil
}

// formatUnixRFC3339 renders a Telegram unix timestamp as an RFC3339 UTC string.
func formatUnixRFC3339(unix int) string {
	return time.Unix(int64(unix), 0).UTC().Format(time.RFC3339)
}

// firstMessageInUpdates returns the ID and date of the first *tg.Message
// carried by an update of one of the given types inside an Updates container,
// or (0, 0) when there is none.
func firstMessageInUpdates(updates tg.UpdatesClass, typeIDs ...uint32) (int, int) {
	u, ok := updates.(*tg.Updates)
	if !ok {
		return 0, 0
	}
	for _, update := range u.Updates {
		if !slices.Contains(typeIDs, update.TypeID()) {
			continue
		}
		carrier, ok := update.(interface{ GetMessage() tg.MessageClass })
		if !ok {
			continue
		}
		if msg, ok := carrier.GetMessage().(*tg.Message); ok {
			return msg.ID, msg.Date
		}
	}
	return 0, 0
}

// floodWaitMessage returns the deterministic retry-after guidance for a wait
// Telegram told a call to take (tgclient.RetryAfter) — including the forms the
// flood-wait middleware wraps when the tool call would wait past its maximum
// or its context ends while it waits — or ok=false when err is no such wait.
// describe renders it both for every tool's failure and for batch handlers
// (e.g. MarkAsRead) that embed it in an aggregated result instead.
//
// What the guidance says follows the wait's scope. An account flood limit is
// cumulative actions over a window, not request rate, so a local rate limiter
// cannot prevent it — the only remedy is to wait the reported duration and
// space the calls out, which the message states so the model stops
// retry-spamming. Slow mode is one chat's limit on sending, and Telegram's
// own delays say only how long to wait.
func floodWaitMessage(tool string, err error) (string, bool) {
	w, ok := tgclient.RetryAfter(err)
	if !ok {
		return "", false
	}
	d := w.Duration.Round(time.Second)
	wait := fmt.Sprintf("%s (%d seconds)", d, int(d/time.Second))
	switch w.Scope {
	case tgclient.ScopeChat:
		return fmt.Sprintf("This chat is in slow mode: wait %s before sending to it again.", wait), true
	case tgclient.ScopeAccount:
		return fmt.Sprintf(
			"Telegram rate-limited this %s call: wait %s before retrying. This is an account-level flood limit (cumulative actions, not request rate), so spacing out %s calls is the only way to avoid it — do not retry immediately.",
			tool, wait, tool,
		), true
	default:
		return fmt.Sprintf("Telegram told this %s call to wait %s before retrying (%s): do not retry sooner.", tool, wait, w.Type), true
	}
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
