package tools

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/summarize"
)

func TestChatSummarizeHandlerValidation(t *testing.T) {
	h := &ChatSummarizeHandler{}
	ctx := context.Background()

	cases := []struct {
		name        string
		in          SummarizeChatInput
		wantErrPart string
	}{
		{
			name:        "zero chat_id",
			in:          SummarizeChatInput{Goal: "summarize"},
			wantErrPart: "chat_id is required",
		},
		{
			name:        "empty goal",
			in:          SummarizeChatInput{ChatID: 1},
			wantErrPart: "goal is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errRes, _, err := h.handle(ctx, nil, tc.in)
			require.NoError(t, err)
			require.NotNil(t, errRes)
			require.True(t, errRes.IsError)
			assert.Contains(t, toolResultText(errRes), tc.wantErrPart)
		})
	}
}

func TestParseSinceTime(t *testing.T) {
	h := &ChatSummarizeHandler{}

	// ISO 8601 date
	got, err := h.parseSinceTime(SummarizeChatInput{Since: "2024-01-15"})
	require.NoError(t, err)
	want := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	assert.True(t, got.Equal(want))

	// RFC3339
	got, err = h.parseSinceTime(SummarizeChatInput{Since: "2024-01-15T12:00:00Z"})
	require.NoError(t, err)
	want = time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	assert.True(t, got.Equal(want))

	// Invalid since
	_, err = h.parseSinceTime(SummarizeChatInput{Since: "not-a-date"})
	require.Error(t, err)

	// Periods
	for _, period := range []string{"day", "week", "month"} {
		before := time.Now()
		got, err := h.parseSinceTime(SummarizeChatInput{Period: period})
		require.NoError(t, err)
		assert.True(t, got.Before(before))
	}

	// Empty period defaults to month (30 days back from now).
	// Bracket with before/after to avoid flakiness near DST transitions.
	beforeCall := time.Now()
	got, err = h.parseSinceTime(SummarizeChatInput{})
	afterCall := time.Now()
	require.NoError(t, err)
	wantLow := beforeCall.Add(-31 * 24 * time.Hour)
	wantHigh := afterCall.Add(-29 * 24 * time.Hour)
	assert.False(t, got.Before(wantLow), "since time %v is too far in the past", got)
	assert.False(t, got.After(wantHigh), "since time %v is too recent", got)

	// Unknown period
	_, err = h.parseSinceTime(SummarizeChatInput{Period: "yearly"})
	require.Error(t, err)
}

func TestCreateProvider(t *testing.T) {
	tests := []struct {
		name        string
		config      summarize.Config
		wantErr     bool
		wantErrPart string
	}{
		{"sampling", summarize.Config{Provider: summarize.ProviderSampling}, false, ""},
		{"empty defaults to sampling", summarize.Config{}, false, ""},
		{"gemini", summarize.Config{Provider: summarize.ProviderGemini, GeminiAPIKey: "test-key"}, false, ""},
		{"gemini missing key", summarize.Config{Provider: summarize.ProviderGemini}, true, "GEMINI_API_KEY is required"},
		{"ollama", summarize.Config{Provider: summarize.ProviderOllama, OllamaURL: "http://localhost:11434"}, false, ""},
		{"ollama missing url", summarize.Config{Provider: summarize.ProviderOllama}, true, "OLLAMA_URL is required"},
		{"anthropic", summarize.Config{Provider: summarize.ProviderAnthropic, AnthropicAPIKey: "test-key"}, false, ""},
		{"anthropic missing key", summarize.Config{Provider: summarize.ProviderAnthropic}, true, "ANTHROPIC_API_KEY is required"},
		{"unknown returns error", summarize.Config{Provider: "openai"}, true, "unknown summarization provider"},
		{"typo returns error", summarize.Config{Provider: "smapling"}, true, "unknown summarization provider"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &ChatSummarizeHandler{config: tt.config}
			got, err := h.createProvider(nil)
			if tt.wantErr {
				require.Error(t, err)
				if tt.wantErrPart != "" {
					assert.Contains(t, err.Error(), tt.wantErrPart)
				}
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
		})
	}
}
