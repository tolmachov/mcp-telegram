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
	MaxMessages int    `json:"max_messages,omitempty" jsonschema:"Maximum messages sent to the summariser (default 2000, hard maximum 10000)."`
	ChatID      int64  `json:"chat_id" jsonschema:"The chat ID to summarize"`
	Goal        string `json:"goal" jsonschema:"What you want from the summary. Examples: 'key points and decisions'\\, 'extract all action items and deadlines'\\, 'analyse sentiment and mood'\\, 'identify top 5 discussed topics'\\, 'create meeting minutes'"`
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
	// Partial is true when the history fetch or summarisation stopped early
	// (e.g. a provider error on a later batch) and Summary covers only the
	// messages or batches done so far; the warning says why.
	Partial bool `json:"partial,omitempty"`
	partialOutcome
}

// Register adds the tool to the MCP server.
func (h *ChatSummarizeHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name:        "SummarizeChat",
		Description: "Use this whenever the user asks to summarise, digest, recap, or 'catch up on' a Telegram chat. Prefer it over fetching messages with GetMessages and summarising them yourself: it performs rolling/incremental summarisation server-side, so it handles long histories (weeks/months, hundreds of messages) without loading every message into the conversation context. Specify a goal (e.g. 'key decisions', 'action items', 'what did I miss') and a time period (day/week/month) or a since date.",
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
		return ErrResult("goal is required"), nil, nil
	}
	maxMessages := in.MaxMessages
	if maxMessages == 0 {
		maxMessages = defaultSummaryMaxMessages
	}
	if maxMessages < 1 || maxMessages > hardSummaryMaxMessages {
		return ErrResult(fmt.Sprintf("max_messages must be between 1 and %d", hardSummaryMaxMessages)), nil, nil
	}

	periodEnd := time.Now()
	since, err := h.parseSinceTime(in)
	if err != nil {
		return ErrResult(fmt.Sprintf("Invalid time parameters: %v", err)), nil, nil
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
	return h.buildResult(in, maxMessages, since, periodEnd, result, err)
}

// buildResult shapes the tool response from a summarizer outcome, kept separate
// from handle so the (result, err) → response branching is unit-testable without
// driving a live LLM. On success it returns the full summary; on a late failure
// that still produced text it returns a partial result (salvaging completed
// batches); and a total failure is returned as the handler error. Whatever
// the summary lacks — messages past maxMessages, history a failed fetch did
// not reach, batches after a failed one — its warning says.
func (h *ChatSummarizeHandler) buildResult(in SummarizeChatInput, maxMessages int, since, periodEnd time.Time, result summarize.Result, err error) (*mcp.CallToolResult, *SummarizeChatResult, error) {
	if err != nil && strings.TrimSpace(result.Summary) == "" {
		return nil, nil, failed(fmt.Sprintf("summarise chat %d", in.ChatID), err)
	}
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
		Partial:           result.Partial || err != nil,
	}
	if result.Truncated {
		out.warn(fmt.Sprintf("The period holds more than max_messages=%d messages; the summary covers only the latest %d.", maxMessages, maxMessages))
	}
	if result.FetchErr != nil {
		out.warnCause("SummarizeChat", "Fetching the period's history stopped early, so the summary covers only the messages fetched before this:", result.FetchErr, "")
	}
	if err != nil {
		// A later batch failed but earlier batches produced a usable summary:
		// return it marked partial rather than throwing the completed work
		// away.
		out.warnCause("SummarizeChat", "Summarisation stopped early, so the summary covers only the batches done before this:", err, "")
	}
	return nil, out, nil
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
