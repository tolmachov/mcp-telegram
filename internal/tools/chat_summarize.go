package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/summarize"
)

// ChatSummarizeHandler handles the SummarizeChat tool
type ChatSummarizeHandler struct {
	msgProvider *messages.Provider
	mcpServer   *server.MCPServer
	config      summarize.Config
}

// NewChatSummarizeHandler creates a new ChatSummarizeHandler
func NewChatSummarizeHandler(msgProvider *messages.Provider, mcpServer *server.MCPServer, config summarize.Config) *ChatSummarizeHandler {
	return &ChatSummarizeHandler{
		msgProvider: msgProvider,
		mcpServer:   mcpServer,
		config:      config,
	}
}

// Tool returns the MCP tool definition
func (h *ChatSummarizeHandler) Tool() mcp.Tool {
	return mcp.NewTool("SummarizeChat",
		mcp.WithDescription("Summarize messages from a Telegram chat using rolling/incremental summarization with AI."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithNumber("chat_id",
			mcp.Description("The chat ID to summarize"),
			mcp.Required(),
		),
		mcp.WithString("goal",
			mcp.Description("What you want from the summary. Examples: 'key points and decisions', 'extract all action items and deadlines', 'analyze sentiment and mood', 'identify top 5 discussed topics', 'create meeting minutes', 'find all decisions made', 'summarize bug discussions', 'track project progress'"),
			mcp.Required(),
		),
		mcp.WithString("period",
			mcp.Description("Time period: 'day', 'week', or 'month' (default: 'month')"),
		),
		mcp.WithString("since",
			mcp.Description("ISO 8601 date to start from (alternative to period, e.g., '2024-01-15')"),
		),
	)
}

// Handle processes the SummarizeChat tool request
func (h *ChatSummarizeHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	chatID := mcp.ParseInt64(request, "chat_id", 0)
	if chatID == 0 {
		return mcp.NewToolResultError("chat_id is required"), nil
	}

	goal := mcp.ParseString(request, "goal", "")
	if goal == "" {
		return mcp.NewToolResultError("goal is required"), nil
	}

	since, err := h.parseSinceTime(request)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Invalid time parameters: %v", err)), nil
	}

	// Create a provider based on configuration
	provider := h.createProvider(ctx)

	summarizer := summarize.NewSummarizer(provider, h.msgProvider, h.config.BatchTokens)

	// Progress callback using MCP notifications
	onProgress := func(current, total int, message string) {
		srv := server.ServerFromContext(ctx)
		if srv != nil {
			_ = srv.SendNotificationToClient(ctx, "notifications/progress", map[string]any{
				"progress": current,
				"total":    total,
				"message":  message,
			})
		}
	}

	result, err := summarizer.Summarize(ctx, chatID, goal, since, onProgress)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Summarization failed: %v", err)), nil
	}

	return mcp.NewToolResultText(result), nil
}

func (h *ChatSummarizeHandler) parseSinceTime(request mcp.CallToolRequest) (time.Time, error) {
	sinceStr := mcp.ParseString(request, "since", "")
	if sinceStr != "" {
		// Try parsing ISO 8601 date
		t, err := time.Parse("2006-01-02", sinceStr)
		if err != nil {
			// Try with time
			t, err = time.Parse(time.RFC3339, sinceStr)
			if err != nil {
				return time.Time{}, fmt.Errorf("invalid since format, use ISO 8601 (e.g., '2024-01-15' or '2024-01-15T00:00:00Z')")
			}
		}
		return t, nil
	}

	period := mcp.ParseString(request, "period", "month")
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

func (h *ChatSummarizeHandler) createProvider(_ context.Context) summarize.Provider {
	switch h.config.Provider {
	case summarize.ProviderSampling:
		return summarize.NewSamplingProvider(h.mcpServer)
	case summarize.ProviderGemini:
		return summarize.NewGeminiProvider(h.config.GeminiAPIKey, h.config.Model)
	case summarize.ProviderOllama:
		return summarize.NewOllamaProvider(h.config.OllamaURL, h.config.Model)
	case summarize.ProviderAnthropic:
		return summarize.NewAnthropicProvider(h.config.AnthropicAPIKey, h.config.Model)
	default:
		// Default to sampling
		return summarize.NewSamplingProvider(h.mcpServer)
	}
}
