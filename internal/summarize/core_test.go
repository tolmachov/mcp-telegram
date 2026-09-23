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
)

type providerFunc func(context.Context, Request) (string, error)

func (f providerFunc) Summarize(ctx context.Context, req Request) (string, error) {
	return f(ctx, req)
}

func TestProviderValidationAndConfigRedaction(t *testing.T) {
	for _, name := range []string{"sampling", "ollama", "gemini", "anthropic"} {
		assert.NoError(t, ValidateProviderName(name))
	}
	assert.Error(t, ValidateProviderName("unknown"))

	cfg := Config{Provider: ProviderGemini, Model: "model", GeminiAPIKey: "secret", BatchTokens: 123}
	for _, rendered := range []string{cfg.String(), fmt.Sprintf("%#v", cfg)} {
		assert.NotContains(t, rendered, "secret")
		assert.Contains(t, rendered, "<redacted>")
		assert.Contains(t, rendered, "<unset>")
	}
}

func TestTokenEstimationAndBatching(t *testing.T) {
	assert.Equal(t, 1, estimateTokens("abcd"))
	assert.Equal(t, 2, estimateTokens("абвг"))
	assert.Nil(t, splitIntoBatchesByTokens(nil, 10))

	input := []messages.Message{
		{ID: 1, Text: strings.Repeat("a", 40)},
		{ID: 2, Text: strings.Repeat("b", 40)},
		{ID: 3, Text: strings.Repeat("c", 40)},
	}
	batches := splitIntoBatchesByTokens(input, 15)
	require.NotEmpty(t, batches)
	count := 0
	for _, batch := range batches {
		assert.NotEmpty(t, batch)
		count += len(batch)
	}
	assert.Equal(t, len(input), count)
}

func TestSummarizeWithProgressSuccessFailurePanicAndCancellation(t *testing.T) {
	req := providerTestRequest()

	s := NewSummarizer(providerFunc(func(context.Context, Request) (string, error) {
		return " summary ", nil
	}), nil, 0)
	assert.Equal(t, DefaultBatchTokens, s.batchTokens)
	got, err := s.summarizeWithProgress(t.Context(), req, 1, 1, nil)
	require.NoError(t, err)
	assert.Equal(t, " summary ", got)

	s.provider = providerFunc(func(context.Context, Request) (string, error) {
		return "", errors.New("provider failed")
	})
	_, err = s.summarizeWithProgress(t.Context(), req, 1, 1, nil)
	assert.ErrorContains(t, err, "provider failed")

	s.provider = providerFunc(func(context.Context, Request) (string, error) {
		panic("provider panic")
	})
	_, err = s.summarizeWithProgress(t.Context(), req, 1, 1, nil)
	assert.ErrorContains(t, err, "provider panicked")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.provider = providerFunc(func(ctx context.Context, _ Request) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	_, err = s.summarizeWithProgress(ctx, req, 1, 1, nil)
	assert.ErrorContains(t, err, "canceled")
}

func TestSamplingProviderWithoutCapabilityAndContentText(t *testing.T) {
	p := NewSamplingProvider(nil)
	_, err := p.Summarize(t.Context(), providerTestRequest())
	assert.ErrorIs(t, err, ErrSamplingUnsupported)
	assert.Equal(t, "hello", contentText(&mcp.TextContent{Text: "hello"}))
	assert.Empty(t, contentText(&mcp.ImageContent{}))
}

func TestSummarizeDetailedCountsCompletedBatches(t *testing.T) {
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
			s := NewSummarizer(llm, messages.NewProviderWithRate(tg.NewClient(inv), 100_000), max(tc.batchTokens, 1))
			got, err := s.SummarizeDetailed(t.Context(), 77, "summarize", time.Time{}, 100, nil)
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
