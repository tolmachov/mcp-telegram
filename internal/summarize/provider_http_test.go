package summarize

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testHTTPClient(t *testing.T, fn roundTripFunc) *http.Client {
	t.Helper()
	return &http.Client{Transport: fn}
}

func TestGeminiProviderSummarizeConcatenatesTextParts(t *testing.T) {
	t.Parallel()

	provider := NewGeminiProvider("test-key", "gemini-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "test-key", req.Header.Get("x-goog-api-key"))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"candidates": [{
					"content": {
						"parts": [
							{"text": "First paragraph."},
							{"text": "  "},
							{"text": "Second paragraph."}
						]
					}
				}]
			}`)),
		}, nil
	})

	summary, err := provider.Summarize(context.Background(), "prompt")
	require.NoError(t, err)
	assert.Equal(t, "First paragraph.\n\nSecond paragraph.", summary)
}

func TestAnthropicProviderSummarizeConcatenatesTextBlocks(t *testing.T) {
	t.Parallel()

	provider := NewAnthropicProvider("test-key", "claude-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "test-key", req.Header.Get("x-api-key"))
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"content": [
					{"type": "text", "text": "First block."},
					{"type": "thinking", "text": "ignored"},
					{"type": "text", "text": "Second block."}
				]
			}`)),
		}, nil
	})

	summary, err := provider.Summarize(context.Background(), "prompt")
	require.NoError(t, err)
	assert.Equal(t, "First block.\n\nSecond block.", summary)
}

// TestAnthropicProviderSummarizeAllWhitespaceBlocks covers the path where every
// content block is whitespace-only — parallel to TestGeminiProviderSummarizeAllWhitespaceParts.
func TestAnthropicProviderSummarizeAllWhitespaceBlocks(t *testing.T) {
	t.Parallel()

	provider := NewAnthropicProvider("test-key", "claude-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"content": [
					{"type": "text", "text": "  "},
					{"type": "text", "text": "\t\n"}
				]
			}`)),
		}, nil
	})

	_, err := provider.Summarize(context.Background(), "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no text content")
}

func TestAnthropicProviderSummarizeErrorsWithoutTextBlocks(t *testing.T) {
	t.Parallel()

	provider := NewAnthropicProvider("test-key", "claude-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"content": [
					{"type": "thinking", "text": "internal"}
				]
			}`)),
		}, nil
	})

	_, err := provider.Summarize(context.Background(), "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no text content")
}

func TestAnthropicProviderSummarizeNonOKStatus(t *testing.T) {
	t.Parallel()

	provider := NewAnthropicProvider("test-key", "claude-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`rate limit exceeded`)),
		}, nil
	})

	_, err := provider.Summarize(context.Background(), "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "429")
}

func TestAnthropicProviderSummarizeInlineAPIError(t *testing.T) {
	t.Parallel()

	provider := NewAnthropicProvider("test-key", "claude-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"error": {"type": "invalid_api_key", "message": "Invalid API key"}
			}`)),
		}, nil
	})

	_, err := provider.Summarize(context.Background(), "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Invalid API key")
}

func TestGeminiProviderSummarizeNonOKStatus(t *testing.T) {
	t.Parallel()

	provider := NewGeminiProvider("test-key", "gemini-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`invalid api key`)),
		}, nil
	})

	_, err := provider.Summarize(context.Background(), "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

// TestGeminiProviderSummarizeInlineAPIError covers the Gemini-specific failure
// mode where the API returns HTTP 200 but embeds an error object in the body
// (e.g. quota exceeded, invalid model). This is different from HTTP-level errors
// and requires a separate code path.
func TestGeminiProviderSummarizeInlineAPIError(t *testing.T) {
	t.Parallel()

	provider := NewGeminiProvider("test-key", "gemini-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"error": {"message": "API quota exceeded", "code": 429}
			}`)),
		}, nil
	})

	_, err := provider.Summarize(context.Background(), "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "API quota exceeded")
	assert.Contains(t, err.Error(), "429")
}

func TestGeminiProviderSummarizeNoCandidates(t *testing.T) {
	t.Parallel()

	provider := NewGeminiProvider("test-key", "gemini-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"candidates": []}`)),
		}, nil
	})

	_, err := provider.Summarize(context.Background(), "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no candidates")
}

// TestGeminiProviderSummarizeAllWhitespaceParts covers the distinct code path
// where parts exist but every part is whitespace-only. This hits the
// len(textParts)==0 check rather than the empty-parts check, and must return
// an error rather than an empty string.
func TestGeminiProviderSummarizeAllWhitespaceParts(t *testing.T) {
	t.Parallel()

	provider := NewGeminiProvider("test-key", "gemini-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"candidates": [{"content": {"parts": [{"text": "  "}, {"text": "\t\n"}]}}]
			}`)),
		}, nil
	})

	_, err := provider.Summarize(context.Background(), "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no text parts")
}

func TestGeminiProviderSummarizeNoParts(t *testing.T) {
	t.Parallel()

	provider := NewGeminiProvider("test-key", "gemini-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"candidates": [{"content": {"parts": []}}]
			}`)),
		}, nil
	})

	_, err := provider.Summarize(context.Background(), "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no parts")
}

func TestAnthropicProviderSummarizeNoContent(t *testing.T) {
	t.Parallel()

	provider := NewAnthropicProvider("test-key", "claude-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"content": []}`)),
		}, nil
	})

	_, err := provider.Summarize(context.Background(), "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no content")
}

func TestAnthropicProviderSummarizeNetworkError(t *testing.T) {
	t.Parallel()

	provider := NewAnthropicProvider("test-key", "claude-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})

	_, err := provider.Summarize(context.Background(), "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sending request")
}

func TestGeminiProviderSummarizeNetworkError(t *testing.T) {
	t.Parallel()

	provider := NewGeminiProvider("test-key", "gemini-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})

	_, err := provider.Summarize(context.Background(), "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sending request")
}

func TestOllamaProviderSummarizeSuccess(t *testing.T) {
	t.Parallel()

	provider := NewOllamaProvider("http://localhost:11434", "llama-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		assert.Equal(t, "/api/generate", req.URL.Path)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"response": "Summary text.",
				"done": true
			}`)),
		}, nil
	})

	summary, err := provider.Summarize(context.Background(), "prompt")
	require.NoError(t, err)
	assert.Equal(t, "Summary text.", summary)
}

func TestOllamaProviderSummarizeNonOKStatus(t *testing.T) {
	t.Parallel()

	provider := NewOllamaProvider("http://localhost:11434", "llama-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`service unavailable`)),
		}, nil
	})

	_, err := provider.Summarize(context.Background(), "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}

func TestOllamaProviderSummarizeInlineError(t *testing.T) {
	t.Parallel()

	provider := NewOllamaProvider("http://localhost:11434", "llama-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"error": "model not found"
			}`)),
		}, nil
	})

	_, err := provider.Summarize(context.Background(), "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "model not found")
}

func TestOllamaProviderSummarizeNetworkError(t *testing.T) {
	t.Parallel()

	provider := NewOllamaProvider("http://localhost:11434", "llama-test")
	provider.client = testHTTPClient(t, func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})

	_, err := provider.Summarize(context.Background(), "prompt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sending request")
}
