package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/tools"
)

// JSON-RPC methods that carry a user request worth logging at Info (with
// redacted params for tools/call; the other three identify their target
// through typed request fields the middleware doesn't read generically).
// Everything else (tools/list, resources/list, prompts/list, initialize,
// notifications, variants negotiation) is high-volume lifecycle noise logged
// at Debug.
var contentMethods = map[string]struct{}{
	methodCallTool:        {},
	"resources/read":      {},
	"prompts/get":         {},
	"completion/complete": {},
}

// safeParamFields are argument names logged verbatim: numeric/opaque
// identifiers, numbers, enums, dates, and booleans. Every other field is free
// text (message bodies, search queries, folder titles, invite links, file
// paths) OR a lookup target that reveals user activity (username, uri) and is
// logged as length only — an allowlist so a new tool's unknown field is
// redacted by default, never leaked. See the tool input structs in
// internal/tools for the field sources.
var safeParamFields = map[string]struct{}{
	// identifiers / opaque handles. username and uri are deliberately absent:
	// on the open deployment they name who/what a user queried, which is
	// activity data, so they are length-redacted like free text.
	"chat_id": {}, "chat_ids": {}, "from_chat_id": {}, "to_chat_id": {},
	"folder_id": {}, "message_id": {}, "offset_id": {}, "top_msg_id": {},
	"reply_to_message_id": {}, "anchor_id": {}, "from_sender_id": {},
	"cursor": {}, "next_cursor": {},
	// numbers / dates / enums
	"limit": {}, "before": {}, "after": {}, "distance": {},
	"period": {}, "since": {}, "from_date": {}, "to_date": {}, "schedule_at": {},
	"mode": {}, "media_type": {}, "kind": {}, "duration_seconds": {},
	// booleans
	"muted": {}, "unread_only": {}, "include_scheduled": {}, "big": {},
	"include_contacts": {}, "include_non_contacts": {}, "include_groups": {},
	"include_channels": {}, "include_bots": {}, "groups": {}, "channels": {},
	"bots": {}, "contacts": {}, "non_contacts": {}, "exclude_muted": {},
	"exclude_read": {}, "exclude_archived": {},
}

// requestLogMiddleware records every JSON-RPC method call server-side — which
// the SDK does not do on its own. It answers three operational questions the
// bare "POST 200" access log cannot: who called (Telegram user id), what they
// called (method + tool + redacted params), and whether an error was returned
// to them (and which). Content methods log at Info (with redacted params for
// tools/call); lifecycle/list methods at Debug. A Go error escalates to Error
// (and, via the GCP handler, to Error Reporting); an IsError tool result — a
// handled failure delivered to the client inside a 200 — logs at Warn with the
// error detail; and a degraded success that set a tools.MetaWarning marker (a
// usable 200 that still hides a failure, e.g. a partial summary) also logs at
// Warn, so it is not buried in an Info line.
//
// Sensitive parameters are never logged verbatim (see safeParamFields): with
// open access the params would otherwise carry arbitrary users' Telegram
// content into Cloud Logging.
func requestLogMiddleware(logger *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			start := time.Now()
			res, err := next(ctx, method, req)
			elapsed := time.Since(start)

			attrs := []any{"method", method, "duration_ms", elapsed.Milliseconds()}
			if u, ok := requestUser(ctx, req); ok {
				attrs = append(attrs, "user_id", u.ID.Int64())
				if u.Username != "" {
					attrs = append(attrs, "username", u.Username)
				}
			}
			attrs = append(attrs, requestAttrs(method, req)...)

			switch {
			case err != nil:
				logger.Error("mcp request failed", append(attrs, "err", err.Error())...)
			case isErrorResult(res):
				logger.Warn("mcp request returned error to client",
					append(attrs, "detail", toolErrorText(res.(*mcp.CallToolResult)))...)
			case resultWarningText(res) != "":
				// A degraded success: the tool returned a usable 200 but flagged
				// a warning (e.g. a partial summary salvaged after a late provider
				// failure). Surface it at Warn so it is not buried in an Info line.
				logger.Warn("mcp request completed with warning",
					append(attrs, "detail", resultWarningText(res))...)
			case isContentMethod(method):
				logger.Info("mcp request", attrs...)
			default:
				logger.Debug("mcp request", attrs...)
			}
			return res, err
		}
	}
}

func isContentMethod(method string) bool {
	_, ok := contentMethods[method]
	return ok
}

// requestUser resolves the authenticated Telegram user for this request. It
// prefers the per-request token the SDK attaches to each server request
// (req.GetExtra().TokenInfo), falling back to the session-frozen token in the
// context. Both yield the same user in the per-user pool (the SDK pins one
// user per session); ok is false in stdio mode (no auth).
func requestUser(ctx context.Context, req mcp.Request) (*authsrv.UserIdentity, bool) {
	if req != nil {
		if extra := req.GetExtra(); extra != nil {
			if u, ok := authsrv.IdentityFromTokenInfo(extra.TokenInfo); ok {
				return u, true
			}
		}
	}
	return authsrv.Identity(ctx)
}

func isErrorResult(res mcp.Result) bool {
	ctr, ok := res.(*mcp.CallToolResult)
	return ok && ctr != nil && ctr.IsError
}

// resultWarningText returns the operator-facing warning a tool attached to a
// non-error result via the tools.MetaWarning key in Meta, or "" when there is
// none. A tool sets it to flag a degraded success (e.g. a partial summary
// salvaged after a later batch failed): the call returns a usable 200 with
// IsError=false, but the failure must still reach the operator log. The SDK
// preserves a handler-set Meta while populating the structured output, so the
// middleware sees it on the in-process result.
func resultWarningText(res mcp.Result) string {
	ctr, ok := res.(*mcp.CallToolResult)
	if !ok || ctr == nil {
		return ""
	}
	if v, ok := ctr.Meta[tools.MetaWarning]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// requestAttrs extracts the method-specific, redaction-aware attributes: the
// tool name and redacted arguments for tools/call, and nothing extra for other
// methods (their identifying params — resource uri, prompt name — arrive as
// typed request fields the middleware cannot read generically; the method name
// plus user id already answer "who did what").
func requestAttrs(method string, req mcp.Request) []any {
	if method != methodCallTool {
		return nil
	}
	ctr, ok := req.(*mcp.CallToolRequest)
	if !ok || ctr.Params == nil {
		return nil
	}
	attrs := []any{"tool", ctr.Params.Name}
	if params := redactArgs(ctr.Params.Arguments); params != nil {
		attrs = append(attrs, "params", params)
	}
	return attrs
}

// maxParamValueLen caps a logged safe value so a pathological argument can't
// flood a log line; longer values are truncated with a marker.
const maxParamValueLen = 200

// redactArgs turns raw tool arguments into a log-safe map: values whose field
// name is in safeParamFields are kept (strings capped at maxParamValueLen),
// every other field is replaced by "<field>_len" carrying only the byte
// length of its raw JSON. Returns nil for empty arguments and a single
// {"unparseable": true} entry when the payload is not a JSON object.
func redactArgs(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return map[string]any{"unparseable": true}
	}
	if len(fields) == 0 {
		return nil
	}
	out := make(map[string]any, len(fields))
	for k, v := range fields {
		if _, safe := safeParamFields[k]; safe {
			out[k] = safeValue(v)
			continue
		}
		out[k+"_len"] = len(v)
	}
	return out
}

// safeValue renders an allowlisted field for logging. Strings are unquoted and
// length-capped; other JSON scalars/containers are passed through as raw text
// (they are ids/numbers/enums/bools/short arrays — never free-form user text).
func safeValue(v json.RawMessage) any {
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		if len(s) > maxParamValueLen {
			return s[:maxParamValueLen] + "…(truncated)"
		}
		return s
	}
	return string(v)
}

// toolErrorText extracts the human-readable message from an error tool result:
// the first TextContent, truncated so a giant payload can't flood the logs.
// Returns "" when the result carries no text content.
func toolErrorText(res *mcp.CallToolResult) string {
	const maxLen = 500
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text := strings.TrimSpace(tc.Text)
			if len(text) > maxLen {
				return text[:maxLen] + "…(truncated)"
			}
			return text
		}
	}
	return ""
}
