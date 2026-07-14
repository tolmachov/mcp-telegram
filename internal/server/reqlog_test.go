package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
	"github.com/tolmachov/mcp-telegram/internal/tools"
)

// callToolReq builds a tools/call request with the given name and raw args.
func callToolReq(name string, args string) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: name, Arguments: json.RawMessage(args)}}
}

// runMiddleware drives requestLogMiddleware around a stub handler and returns
// the captured log output. ctx carries an authenticated identity when userID>0.
func runMiddleware(t *testing.T, level slog.Leveler, userID tgid.UserID, method string, req mcp.Request, res mcp.Result, err error) string {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}))
	// Attach identity the way the SDK does: on the per-request Extra.
	if userID > 0 {
		if ctr, ok := req.(*mcp.CallToolRequest); ok {
			ctr.Extra = &mcp.RequestExtra{
				TokenInfo: authsrv.NewTokenInfoForTesting(userID, "durov", time.Now().Add(time.Hour)),
			}
		}
	}
	handler := requestLogMiddleware(logger)(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return res, err
	})
	_, _ = handler(t.Context(), method, req)
	return buf.String()
}

func TestRequestLogUserID(t *testing.T) {
	out := runMiddleware(t, slog.LevelInfo, 424242, methodCallTool,
		callToolReq("GetChats", `{}`),
		&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil)
	if !strings.Contains(out, "user_id=424242") {
		t.Errorf("expected user_id in log, got: %q", out)
	}
	if !strings.Contains(out, "tool=GetChats") {
		t.Errorf("expected tool name in log, got: %q", out)
	}
	if !strings.Contains(out, "level=INFO") {
		t.Errorf("content method should log at Info, got: %q", out)
	}
	if !strings.Contains(out, "duration_ms=") {
		t.Errorf("expected duration_ms, got: %q", out)
	}
}

func TestRequestLogRedactsSensitiveParams(t *testing.T) {
	args := `{"chat_id":123,"limit":50,"message":"secret plaintext body","query":"private search","username":"durov_secret"}`
	out := runMiddleware(t, slog.LevelInfo, 1, methodCallTool,
		callToolReq("SendMessage", args),
		&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "sent"}}}, nil)

	if strings.Contains(out, "secret plaintext body") || strings.Contains(out, "private search") {
		t.Errorf("sensitive free-text leaked into log: %q", out)
	}
	// A looked-up username is activity data, not an opaque id: length only.
	if strings.Contains(out, "durov_secret") {
		t.Errorf("username lookup target leaked into log: %q", out)
	}
	// Safe fields kept; sensitive fields present only as lengths.
	for _, want := range []string{"chat_id:123", "limit:50", "message_len:", "query_len:", "username_len:"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in redacted params, got: %q", want, out)
		}
	}
}

func TestRequestLogUnknownFieldRedactedByDefault(t *testing.T) {
	// A field not in the allowlist must be length-only, never verbatim.
	out := runMiddleware(t, slog.LevelInfo, 1, methodCallTool,
		callToolReq("FutureTool", `{"new_secret_field":"do-not-log-this"}`),
		&mcp.CallToolResult{}, nil)
	if strings.Contains(out, "do-not-log-this") {
		t.Errorf("unknown field logged verbatim: %q", out)
	}
	if !strings.Contains(out, "new_secret_field_len:") {
		t.Errorf("unknown field not length-redacted: %q", out)
	}
}

func TestRequestLogErrorResultWarns(t *testing.T) {
	out := runMiddleware(t, slog.LevelInfo, 42, methodCallTool,
		callToolReq("SummarizeChat", `{"chat_id":5,"goal":"tl;dr"}`),
		&mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "gemini: model not found (code: 404)"}}}, nil)
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("error result should log at Warn, got: %q", out)
	}
	if !strings.Contains(out, "404") || !strings.Contains(out, "SummarizeChat") {
		t.Errorf("expected error detail and tool name, got: %q", out)
	}
	if !strings.Contains(out, "user_id=42") {
		t.Errorf("expected user_id on error result, got: %q", out)
	}
}

func TestRequestLogDegradedSuccessWarns(t *testing.T) {
	// A non-error result (IsError=false) carrying a MetaWarning marker — a
	// partial summary after a late provider failure — must escalate to Warn,
	// not be buried at Info. This is the motivating bug's surviving edge.
	res := &mcp.CallToolResult{
		Meta:    mcp.Meta{tools.MetaWarning: "summarization stopped early: gemini: 503"},
		Content: []mcp.Content{&mcp.TextContent{Text: "partial summary"}},
	}
	out := runMiddleware(t, slog.LevelInfo, 42, methodCallTool,
		callToolReq("SummarizeChat", `{"chat_id":5,"goal":"tl;dr"}`), res, nil)
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("degraded success should log at Warn, got: %q", out)
	}
	if !strings.Contains(out, "stopped early") || !strings.Contains(out, "503") {
		t.Errorf("expected the warning detail in the log, got: %q", out)
	}
	if !strings.Contains(out, "user_id=42") {
		t.Errorf("expected user_id on the warning, got: %q", out)
	}
}

func TestRequestLogGoErrorEscalates(t *testing.T) {
	out := runMiddleware(t, slog.LevelInfo, 1, methodCallTool,
		callToolReq("GetChats", `{}`), nil, errors.New("transport exploded"))
	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("go error should log at Error, got: %q", out)
	}
	if !strings.Contains(out, "transport exploded") {
		t.Errorf("expected error text, got: %q", out)
	}
}

func TestRequestLogServiceMethodAtDebug(t *testing.T) {
	// tools/list is lifecycle noise → Debug: nothing at Info level.
	res := &mcp.ListToolsResult{}
	if out := runMiddleware(t, slog.LevelInfo, 1, methodListTools, callToolReq("", ``), res, nil); out != "" {
		t.Errorf("service method must not log at Info level, got: %q", out)
	}
	// At Debug level it appears.
	if out := runMiddleware(t, slog.LevelDebug, 1, methodListTools, callToolReq("", ``), res, nil); !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "method=tools/list") {
		t.Errorf("service method should log at Debug, got: %q", out)
	}
}

func TestRequestLogNoIdentityInStdio(t *testing.T) {
	// No TokenInfo in ctx (stdio mode) → no user_id attr, but still logs.
	out := runMiddleware(t, slog.LevelInfo, 0, methodCallTool,
		callToolReq("GetChats", `{}`),
		&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil)
	if strings.Contains(out, "user_id=") {
		t.Errorf("stdio mode should not log a user_id, got: %q", out)
	}
	if !strings.Contains(out, "tool=GetChats") {
		t.Errorf("expected the call to still be logged, got: %q", out)
	}
}

func TestRedactArgsUnparseable(t *testing.T) {
	if got := redactArgs(json.RawMessage(`not json`)); got["unparseable"] != true {
		t.Errorf("expected unparseable marker, got: %v", got)
	}
	if got := redactArgs(nil); got != nil {
		t.Errorf("empty args should yield nil, got: %v", got)
	}
	if got := redactArgs(json.RawMessage(`{}`)); got != nil {
		t.Errorf("empty object should yield nil, got: %v", got)
	}
}

func TestToolErrorTextTruncates(t *testing.T) {
	long := strings.Repeat("x", 1000)
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: long}}}
	got := toolErrorText(res)
	if len(got) > 600 || !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("toolErrorText did not truncate: len=%d", len(got))
	}
	if got := toolErrorText(&mcp.CallToolResult{}); got != "" {
		t.Errorf("empty content should yield empty string, got %q", got)
	}
}
