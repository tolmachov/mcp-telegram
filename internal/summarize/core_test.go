package summarize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	telegramfake "github.com/tolmachov/mcp-telegram/internal/testutil/telegram"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

type providerFunc func(context.Context, Request) (string, error)

func (f providerFunc) Summarize(ctx context.Context, req Request) (string, error) {
	return f(ctx, req)
}

// fixedSummarizer summarizes through llm with the given batch budget.
func fixedSummarizer(llm Provider, batchTokens int) *Summarizer {
	return &Summarizer{name: ProviderSampling, providerFor: fixedProvider(llm), batchTokens: batchTokens}
}

// key serves a fixed API key.
func key(value string) func() (string, error) {
	return func() (string, error) { return value, nil }
}

// unread fails the test if New reads a key the provider does not use.
func unread(t *testing.T) func() (string, error) {
	return func() (string, error) {
		t.Error("read an API key the configured provider does not use")
		return "", nil
	}
}

func TestNewValidatesConfig(t *testing.T) {
	readErr := errors.New("keychain access denied")
	tests := []struct {
		name        string
		config      Config
		wantErrPart string
	}{
		{"sampling", Config{Provider: ProviderSampling, BatchTokens: 1, GeminiAPIKey: unread(t), AnthropicAPIKey: unread(t)}, ""},
		{"gemini", Config{Provider: ProviderGemini, BatchTokens: 1, GeminiAPIKey: key("test-key"), AnthropicAPIKey: unread(t)}, ""},
		{"gemini missing key", Config{Provider: ProviderGemini, BatchTokens: 1, GeminiAPIKey: key("")}, "MCP_SUMMARIZE_GEMINI_API_KEY is required"},
		{"gemini key unreadable", Config{Provider: ProviderGemini, BatchTokens: 1, GeminiAPIKey: func() (string, error) { return "", readErr }}, "keychain access denied"},
		{"ollama", Config{Provider: ProviderOllama, BatchTokens: 1, OllamaURL: "http://localhost:11434", GeminiAPIKey: unread(t), AnthropicAPIKey: unread(t)}, ""},
		{"ollama missing url", Config{Provider: ProviderOllama, BatchTokens: 1}, "OLLAMA_URL is required"},
		{"anthropic", Config{Provider: ProviderAnthropic, BatchTokens: 1, AnthropicAPIKey: key("test-key"), GeminiAPIKey: unread(t)}, ""},
		{"anthropic missing key", Config{Provider: ProviderAnthropic, BatchTokens: 1, AnthropicAPIKey: key("")}, "MCP_SUMMARIZE_ANTHROPIC_API_KEY is required"},
		{"empty", Config{BatchTokens: 1}, "invalid summarization provider"},
		{"unknown", Config{Provider: "openai", BatchTokens: 1}, "invalid summarization provider"},
		{"zero batch tokens", Config{Provider: ProviderSampling}, "batch-tokens must be positive"},
		{"negative batch tokens", Config{Provider: ProviderSampling, BatchTokens: -5}, "batch-tokens must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := New(tt.config)
			if tt.wantErrPart != "" {
				assert.ErrorContains(t, err, tt.wantErrPart)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.config.Provider, s.ProviderName())
			assert.NotNil(t, s.providerFor(nil))
		})
	}
}

func TestSamplingBindsToTheCallSession(t *testing.T) {
	s, err := New(Config{Provider: ProviderSampling, BatchTokens: 1})
	require.NoError(t, err)
	_, err = s.providerFor(nil).Summarize(t.Context(), providerTestRequest())
	assert.ErrorIs(t, err, ErrSamplingUnsupported)
}

func TestTokenEstimationAndBatching(t *testing.T) {
	assert.Equal(t, 1, estimateTokens([]byte("abcd")))
	assert.Equal(t, 2, estimateTokens([]byte("абвг")))
	empty, err := splitIntoBatchesByTokens(nil, 10)
	require.NoError(t, err)
	assert.Nil(t, empty)

	input := []messages.Message{
		{ID: 1, Text: strings.Repeat("a", 40)},
		{ID: 2, Text: strings.Repeat("b", 40)},
		{ID: 3, Text: strings.Repeat("c", 40)},
	}
	batches, err := splitIntoBatchesByTokens(input, 15)
	require.NoError(t, err)
	require.NotEmpty(t, batches)
	count := 0
	for _, batch := range batches {
		assert.NotEmpty(t, batch)
		count += len(batch)
	}
	assert.Equal(t, len(input), count)
}

// TestBatchingEstimatesTheEncodedBytes pins that the budget is spent on what
// the provider receives: each batch's JSON, not a shorter rendering of it.
func TestBatchingEstimatesTheEncodedBytes(t *testing.T) {
	input := []messages.Message{
		{ID: 1, SenderName: strings.Repeat("n", 200), Text: "a"},
		{ID: 2, SenderName: strings.Repeat("n", 200), Text: "b"},
	}
	encoded, err := json.Marshal(input[0])
	require.NoError(t, err)
	perMessage := estimateTokens(encoded)

	batches, err := splitIntoBatchesByTokens(input, perMessage)
	require.NoError(t, err)
	assert.Len(t, batches, 2, "two messages must not fit a one-message budget")

	batches, err = splitIntoBatchesByTokens(input, 2*perMessage)
	require.NoError(t, err)
	require.Len(t, batches, 1)
	sent, err := json.Marshal(batches[0])
	require.NoError(t, err)
	want, err := json.Marshal(input)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(sent))
}

func TestSummarizeWithProgressSuccessFailurePanicAndCancellation(t *testing.T) {
	req := providerTestRequest()

	got, err := summarizeWithProgress(t.Context(), providerFunc(func(context.Context, Request) (string, error) {
		return " summary ", nil
	}), req, 1, 1, nil)
	require.NoError(t, err)
	assert.Equal(t, " summary ", got)

	_, err = summarizeWithProgress(t.Context(), providerFunc(func(context.Context, Request) (string, error) {
		return "", errors.New("provider failed")
	}), req, 1, 1, nil)
	assert.ErrorContains(t, err, "provider failed")

	_, err = summarizeWithProgress(t.Context(), providerFunc(func(context.Context, Request) (string, error) {
		panic("provider panic")
	}), req, 1, 1, nil)
	assert.ErrorContains(t, err, "provider panicked")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = summarizeWithProgress(ctx, providerFunc(func(ctx context.Context, _ Request) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}), req, 1, 1, nil)
	assert.ErrorContains(t, err, "canceled")
}

func TestSamplingProviderWithoutCapabilityAndContentText(t *testing.T) {
	p := NewSamplingProvider(nil)
	_, err := p.Summarize(t.Context(), providerTestRequest())
	assert.ErrorIs(t, err, ErrSamplingUnsupported)
	assert.Equal(t, "hello", contentText(&mcp.TextContent{Text: "hello"}))
	assert.Empty(t, contentText(&mcp.ImageContent{}))
}

func TestSummarizeCountsCompletedBatches(t *testing.T) {
	for _, tc := range []struct {
		name        string
		texts       []string
		batchTokens int
		failBatch   int
		processed   int
		summary     string
	}{
		{name: "all batches", texts: []string{"third", "", "second", "first"}, processed: 3, summary: "first second third"},
		{name: "several messages in one batch", texts: []string{"third", "", "second", "first"}, batchTokens: 1000, processed: 3, summary: "first second third"},
		{name: "later failure", texts: []string{"third", "", "second", "first"}, failBatch: 2, processed: 1, summary: "first"},
		{name: "first failure", texts: []string{"second", "first"}, failBatch: 1},
		{name: "media only", texts: []string{"", ""}, summary: "No text messages found in the specified period."},
		{name: "empty", summary: "No messages found in the specified period."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := telegramfake.New(
				telegramfake.Typed(func(_ context.Context, _ *tg.UsersGetUsersRequest, out *tg.UserClassVector) error {
					out.Elems = nil // not a user: fall through to the channel probe
					return nil
				}),
				telegramfake.Typed(func(_ context.Context, _ *tg.ChannelsGetChannelsRequest, out *tg.MessagesChatsBox) error {
					out.Chats = &tg.MessagesChats{Chats: []tg.ChatClass{&tg.Channel{ID: 77, AccessHash: 100}}}
					return nil
				}),
				telegramfake.Typed(func(_ context.Context, _ *tg.MessagesGetHistoryRequest, out *tg.MessagesMessagesBox) error {
					raw := make([]tg.MessageClass, 0, len(tc.texts))
					for i, text := range tc.texts {
						msg := &tg.Message{ID: len(tc.texts) - i, Date: 100 - i, Message: text}
						if text == "" {
							msg.Media = &tg.MessageMediaPhoto{}
						}
						raw = append(raw, msg)
					}
					out.Messages = &tg.MessagesMessages{Messages: raw}
					return nil
				}),
			)
			calls := 0
			llm := providerFunc(func(_ context.Context, req Request) (string, error) {
				calls++
				if calls == tc.failBatch {
					return "", assert.AnError
				}
				var batch []messages.Message
				if err := json.Unmarshal(req.Messages, &batch); err != nil {
					return "", fmt.Errorf("decoding test batch: %w", err)
				}
				summary := req.PreviousSummary
				for _, msg := range batch {
					summary += " " + msg.Text
				}
				return strings.TrimSpace(summary), nil
			})
			s := fixedSummarizer(llm, max(tc.batchTokens, 1))
			got, err := s.Summarize(t.Context(), nil, messages.NewProvider(tgclient.NewResolver(t.Context(), tg.NewClient(inv)), 100_000), 77, "summarize", time.Time{}, 100, nil)
			if tc.failBatch > 0 {
				require.ErrorIs(t, err, assert.AnError)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.processed, got.MessagesProcessed)
			assert.Equal(t, tc.summary, got.Summary)
			assert.Equal(t, tc.failBatch > 0, got.Partial)
			assert.False(t, got.Truncated)
			assert.Zero(t, inv.Remaining())
		})
	}
}

func TestPeriod(t *testing.T) {
	assert.Equal(t, []string{"day", "week", "month"}, PeriodNames())
	d, ok := Period("week")
	require.True(t, ok)
	assert.Equal(t, 7*24*time.Hour, d)
	_, ok = Period("year")
	assert.False(t, ok)
}
