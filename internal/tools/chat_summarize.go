package tools

import (
	"context"
	"errors"
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
	config      summarize.Config
}

// NewChatSummarizeHandler creates a new ChatSummarizeHandler.
func NewChatSummarizeHandler(msgProvider *messages.Provider, config summarize.Config) *ChatSummarizeHandler {
	return &ChatSummarizeHandler{
		msgProvider: msgProvider,
		config:      config,
	}
}

// SummarizeChatInput is the input for the SummarizeChat tool.
type SummarizeChatInput struct {
	ChatID int64  `json:"chat_id" jsonschema:"The chat ID to summarize"`
	Goal   string `json:"goal" jsonschema:"What you want from the summary. Examples: 'key points and decisions'\\, 'extract all action items and deadlines'\\, 'analyze sentiment and mood'\\, 'identify top 5 discussed topics'\\, 'create meeting minutes'"`
	Period string `json:"period,omitempty" jsonschema:"Time period: 'day'\\, 'week'\\, or 'month' (default: 'month')"`
	Since  string `json:"since,omitempty" jsonschema:"Date in YYYY-MM-DD or RFC3339 format to start from (alternative to period\\, e.g.\\, '2024-01-15')"`
}

// SummarizeChatResult is the typed output of SummarizeChat. Clients get
// the summary text plus the analysis window so they can render provenance
// without re-computing the period from the input.
type SummarizeChatResult struct {
	ChatID      int64  `json:"chat_id"`
	Goal        string `json:"goal"`
	Period      string `json:"period,omitempty"`
	PeriodStart string `json:"period_start"` // RFC3339
	PeriodEnd   string `json:"period_end"`   // RFC3339
	Provider    string `json:"provider"`
	Summary     string `json:"summary"`
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
	mcp.AddTool(s, &mcp.Tool{
		Name:        "SummarizeChat",
		Description: "Use this whenever the user asks to summarize, digest, recap, or 'catch up on' a Telegram chat. Prefer it over fetching messages with GetMessages and summarizing them yourself: it performs rolling/incremental summarization server-side, so it handles long histories (weeks/months, hundreds of messages) without loading every message into the conversation context. Specify a goal (e.g. 'key decisions', 'action items', 'what did I miss') and a time period (day/week/month) or a since date.",
		InputSchema: inputSchemaWithEnums[SummarizeChatInput](map[string][]any{
			"period": {"day", "week", "month"},
		}),
		// Note: ReadOnlyHint is intentionally NOT set. The tool calls out
		// to external LLM providers (sampling, Gemini, Ollama, Anthropic)
		// which may cache, log, or bill for the content — it is not a
		// pure read of Telegram state. OpenWorldHint reflects that.
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *ChatSummarizeHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in SummarizeChatInput) (*mcp.CallToolResult, *SummarizeChatResult, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}
	if in.Goal == "" {
		return errResult("goal is required"), nil, nil
	}

	periodEnd := time.Now()
	since, err := h.parseSinceTime(in)
	if err != nil {
		return errResult(fmt.Sprintf("Invalid time parameters: %v", err)), nil, nil
	}

	provider, err := h.createProvider(req.Session)
	if err != nil {
		return errResult(fmt.Sprintf("summarization misconfigured: %v", err)), nil, nil
	}
	summarizer := summarize.NewSummarizer(provider, h.msgProvider, h.config.BatchTokens)

	mcpLog(ctx, req.Session, logLevelInfo, "SummarizeChat", map[string]any{
		"chat_id":  in.ChatID,
		"goal":     in.Goal,
		"since":    since.Format(time.RFC3339),
		"provider": h.config.Provider,
	})

	// Progress callback using MCP notifications. sendProgress is a no-op when
	// the client did not provide a progressToken.
	onProgress := func(current, total int, message string) {
		sendProgress(ctx, req, float64(current), float64(total), message)
	}

	result, err := summarizer.Summarize(ctx, in.ChatID, in.Goal, since, onProgress)
	if err != nil {
		mcpLog(ctx, req.Session, logLevelError, "SummarizeChat", map[string]any{
			"chat_id": in.ChatID,
			"error":   err.Error(),
		})
	}
	errRes, out := h.buildResult(in, since, periodEnd, result, err)
	return errRes, out, nil
}

// buildResult shapes the tool response from a summarizer outcome, kept separate
// from handle so the (result, err) → response branching is unit-testable without
// driving a live LLM. On success it returns the full summary; on a late failure
// that still produced text it returns a partial result (salvaging completed
// batches); the sampling-unsupported case gets a targeted error; and a total
// failure returns a generic error.
func (h *ChatSummarizeHandler) buildResult(in SummarizeChatInput, since, periodEnd time.Time, result string, err error) (*mcp.CallToolResult, *SummarizeChatResult) {
	out := &SummarizeChatResult{
		ChatID:      in.ChatID,
		Goal:        in.Goal,
		Period:      in.Period,
		PeriodStart: since.UTC().Format(time.RFC3339),
		PeriodEnd:   periodEnd.UTC().Format(time.RFC3339),
		Provider:    string(h.config.Provider),
		Summary:     result,
	}

	if err == nil {
		return nil, out
	}
	// Specifically surface the sampling-unsupported case so the user gets a
	// clear next step instead of a generic transport error.
	if errors.Is(err, summarize.ErrSamplingUnsupported) {
		return errResult(err.Error()), nil
	}
	// A later batch failed but earlier batches produced a usable summary —
	// return it marked partial rather than throwing the completed work away.
	// The warning also rides in Meta so the server request logger surfaces
	// this degraded success at Warn (the result itself is not an error).
	if strings.TrimSpace(result) != "" {
		out.Partial = true
		out.Warning = fmt.Sprintf("summarization stopped early: %v", err)
		return &mcp.CallToolResult{Meta: mcp.Meta{MetaWarning: out.Warning}}, out
	}
	return errResult(fmt.Sprintf("Summarization failed: %v", err)), nil
}

func (h *ChatSummarizeHandler) parseSinceTime(in SummarizeChatInput) (time.Time, error) {
	if in.Since != "" {
		// Try parsing ISO 8601 date.
		t, err := time.Parse("2006-01-02", in.Since)
		if err != nil {
			t, err = time.Parse(time.RFC3339, in.Since)
			if err != nil {
				return time.Time{}, fmt.Errorf("invalid since format, use ISO 8601 (e.g., '2024-01-15' or '2024-01-15T00:00:00Z')")
			}
		}
		return t, nil
	}

	period := in.Period
	if period == "" {
		period = "month"
	}
	now := time.Now()
	switch period {
	case "day":
		return now.Add(-24 * time.Hour), nil
	case "week":
		return now.Add(-7 * 24 * time.Hour), nil
	case "month":
		return now.Add(-30 * 24 * time.Hour), nil
	default:
		return time.Time{}, fmt.Errorf("invalid period: %s (use 'day', 'week', or 'month')", period)
	}
}

func (h *ChatSummarizeHandler) createProvider(session *mcp.ServerSession) (summarize.Provider, error) {
	switch h.config.Provider {
	case "", summarize.ProviderSampling:
		// Empty string is the zero value of ProviderName; treat it as the
		// default (sampling) so programmatic Config{} works without surprises.
		return summarize.NewSamplingProvider(session), nil
	case summarize.ProviderGemini:
		if h.config.GeminiAPIKey == "" {
			return nil, fmt.Errorf("GEMINI_API_KEY is required when using --summarize-provider=gemini")
		}
		return summarize.NewGeminiProvider(h.config.GeminiAPIKey, h.config.Model), nil
	case summarize.ProviderOllama:
		if h.config.OllamaURL == "" {
			return nil, fmt.Errorf("OLLAMA_URL is required when using --summarize-provider=ollama")
		}
		return summarize.NewOllamaProvider(h.config.OllamaURL, h.config.Model), nil
	case summarize.ProviderAnthropic:
		if h.config.AnthropicAPIKey == "" {
			return nil, fmt.Errorf("ANTHROPIC_API_KEY is required when using --summarize-provider=anthropic")
		}
		return summarize.NewAnthropicProvider(h.config.AnthropicAPIKey, h.config.Model), nil
	default:
		return nil, fmt.Errorf("unknown summarization provider %q; expected one of: sampling, gemini, ollama, anthropic", h.config.Provider)
	}
}
