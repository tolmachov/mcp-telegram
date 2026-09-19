package summarize

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/messages"
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
