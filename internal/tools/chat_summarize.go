package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/summarize"
)

// ChatSummarizeHandler handles the SummarizeChat tool.
type ChatSummarizeHandler struct {
	msgProvider *messages.Provider
	summarizer  *summarize.Summarizer
}

// NewChatSummarizeHandler creates a new ChatSummarizeHandler. When
// summarisation is misconfigured, summarizer is one built by
// summarize.Unavailable: the tool stays registered and every call reports
// why, so the other tools keep working.
func NewChatSummarizeHandler(msgProvider *messages.Provider, summarizer *summarize.Summarizer) *ChatSummarizeHandler {
	return &ChatSummarizeHandler{
		msgProvider: msgProvider,
		summarizer:  summarizer,
	}
}

// SummarizeChatInput is the input for the SummarizeChat tool.
type SummarizeChatInput struct {
	MaxMessages int    `json:"max_messages,omitempty" jsonschema:"Maximum messages sent to the summarizer (default 2000, hard maximum 10000)."`
	ChatID      int64  `json:"chat_id" jsonschema:"The chat ID to summarize"`
	Goal        string `json:"goal" jsonschema:"What you want from the summary. Examples: 'key points and decisions'\\, 'extract all action items and deadlines'\\, 'analyze sentiment and mood'\\, 'identify top 5 discussed topics'\\, 'create meeting minutes'"`
	Period      string `json:"period,omitempty" jsonschema:"Time period to look back over (default: 'month')"`
	Since       string `json:"since,omitempty" jsonschema:"Date to start from (alternative to period): YYYY-MM-DD or YYYY-MM-DD HH:MM:SS in UTC\\, or RFC3339\\, e.g. '2024-01-15'"`
}

const (
	defaultSummaryMaxMessages = 2000
	hardSummaryMaxMessages    = 10000
)

// SummarizeChatResult is the typed output of SummarizeChat. Clients get
// the summary text plus the analysis window so they can render provenance
// without re-computing the period from the input.
type SummarizeChatResult struct {
	ChatID            int64  `json:"chat_id"`
	Goal              string `json:"goal"`
	Period            string `json:"period,omitempty"`
	PeriodStart       string `json:"period_start"` // RFC3339
	PeriodEnd         string `json:"period_end"`   // RFC3339
	Provider          string `json:"provider"`
	Summary           string `json:"summary"`
	MessagesProcessed int    `json:"messages_processed"`
	Truncated         bool   `json:"truncated"`
	// Partial is true when summarization stopped early (e.g. a provider error
	// on a later batch) and Summary holds only the batches completed so far.
	Partial bool   `json:"partial,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// MetaWarning is the CallToolResult.Meta key a tool sets to flag a degraded
// success — a usable 200 (IsError=false) that nonetheless hides a failure the
// operator should see. The server's request logger reads it and escalates the
// entry to Warn (see internal/server/reqlog.go); without it a partial result
// would be logged only at Info server-side and the failure would be invisible.
const MetaWarning = "mcp-telegram/warning"

// Register adds the tool to the MCP server.
func (h *ChatSummarizeHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name:        "SummarizeChat",
		Description: "Use this whenever the user asks to summarize, digest, recap, or 'catch up on' a Telegram chat. Prefer it over fetching messages with GetMessages and summarizing them yourself: it performs rolling/incremental summarization server-side, so it handles long histories (weeks/months, hundreds of messages) without loading every message into the conversation context. Specify a goal (e.g. 'key decisions', 'action items', 'what did I miss') and a time period (day/week/month) or a since date.",
		InputSchema: inputSchemaWithEnums[SummarizeChatInput](map[string][]string{
			"period": summarize.PeriodNames(),
		}),
		// Note: ReadOnlyHint is intentionally NOT set. The tool calls out
		// to external LLM providers (sampling, Gemini, Ollama, Anthropic)
		// which may cache, log, or bill for the content — it is not a
		// pure read of Telegram state. OpenWorldHint reflects that.
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *ChatSummarizeHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in SummarizeChatInput) (*mcp.CallToolResult, *SummarizeChatResult, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}
	if in.Goal == "" {
		return errResult("goal is required"), nil, nil
	}
	maxMessages := in.MaxMessages
	if maxMessages == 0 {
		maxMessages = defaultSummaryMaxMessages
	}
	if maxMessages < 1 || maxMessages > hardSummaryMaxMessages {
		return errResult(fmt.Sprintf("max_messages must be between 1 and %d", hardSummaryMaxMessages)), nil, nil
	}

	periodEnd := time.Now()
	since, err := h.parseSinceTime(in)
	if err != nil {
		return errResult(fmt.Sprintf("Invalid time parameters: %v", err)), nil, nil
	}

	mcpLog(ctx, req.Session, logLevelInfo, "SummarizeChat", map[string]any{
		"chat_id":  in.ChatID,
		"goal":     in.Goal,
		"since":    since.Format(time.RFC3339),
		"provider": h.summarizer.ProviderName(),
	})

	// Progress callback using MCP notifications. sendProgress is a no-op when
	// the client did not provide a progressToken.
	onProgress := func(current, total int, message string) {
		sendProgress(ctx, req, float64(current), float64(total), message)
	}

	result, err := h.summarizer.Summarize(ctx, req.Session, h.msgProvider, in.ChatID, in.Goal, since, maxMessages, onProgress)
	return h.buildResult(in, since, periodEnd, result, err)
}

// buildResult shapes the tool response from a summarizer outcome, kept separate
// from handle so the (result, err) → response branching is unit-testable without
// driving a live LLM. On success it returns the full summary; on a late failure
// that still produced text it returns a partial result (salvaging completed
// batches); and a total failure is returned as the handler error.
func (h *ChatSummarizeHandler) buildResult(in SummarizeChatInput, since, periodEnd time.Time, result summarize.Result, err error) (*mcp.CallToolResult, *SummarizeChatResult, error) {
	out := &SummarizeChatResult{
		ChatID:            in.ChatID,
		Goal:              in.Goal,
		Period:            in.Period,
		PeriodStart:       since.UTC().Format(time.RFC3339),
		PeriodEnd:         periodEnd.UTC().Format(time.RFC3339),
		Provider:          string(h.summarizer.ProviderName()),
		Summary:           result.Summary,
		MessagesProcessed: result.MessagesProcessed,
		Truncated:         result.Truncated,
		Partial:           result.Partial,
		Warning:           result.Warning,
	}

	if err == nil {
		if out.Partial || out.Truncated {
			return &mcp.CallToolResult{Meta: mcp.Meta{MetaWarning: out.Warning}}, out, nil
		}
		return nil, out, nil
	}
	// A later batch failed but earlier batches produced a usable summary —
	// return it marked partial rather than throwing the completed work away.
	// The warning also rides in Meta so the server request logger surfaces
	// this degraded success at Warn (the result itself is not an error).
	if strings.TrimSpace(result.Summary) != "" {
		out.Partial = true
		out.Warning = fmt.Sprintf("summarization stopped early: %v", err)
		return &mcp.CallToolResult{Meta: mcp.Meta{MetaWarning: out.Warning}}, out, nil
	}
	return nil, nil, failed(fmt.Sprintf("summarize chat %d", in.ChatID), err)
}

func (h *ChatSummarizeHandler) parseSinceTime(in SummarizeChatInput) (time.Time, error) {
	if in.Since != "" {
		return parseDate(in.Since)
	}

	period := in.Period
	if period == "" {
		period = "month"
	}
	d, ok := summarize.Period(period)
	if !ok {
		return time.Time{}, fmt.Errorf("invalid period: %s (use one of: %s)", period, strings.Join(summarize.PeriodNames(), ", "))
	}
	return time.Now().Add(-d), nil
}
