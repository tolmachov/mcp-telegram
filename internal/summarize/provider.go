package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Provider is an interface for LLM providers that can summarize text.
type Provider interface {
	Summarize(ctx context.Context, req Request) (string, error)
}

// Request keeps trusted instructions separate from untrusted chat data. The
// Messages field must contain a JSON array; providers place System in their
// native system channel and never concatenate chat text into it.
type Request struct {
	System          string
	Goal            string
	PreviousSummary string
	Messages        json.RawMessage
}

func (r Request) userContent() (string, error) {
	if !json.Valid(r.Messages) {
		return "", fmt.Errorf("messages is not valid JSON")
	}
	payload := struct {
		Goal            string          `json:"goal"`
		PreviousSummary string          `json:"previous_summary,omitempty"`
		Messages        json.RawMessage `json:"messages"`
	}{r.Goal, r.PreviousSummary, r.Messages}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshaling provider input: %w", err)
	}
	return string(raw), nil
}

// ProviderName represents a valid summarization provider name.
type ProviderName string

const (
	ProviderSampling  ProviderName = "sampling"
	ProviderOllama    ProviderName = "ollama"
	ProviderGemini    ProviderName = "gemini"
	ProviderAnthropic ProviderName = "anthropic"
)

// Config holds configuration for summarization providers.
type Config struct {
	Provider  ProviderName // "sampling", "ollama", "gemini", or "anthropic"
	Model     string       // provider-specific model name
	OllamaURL string       // URL for Ollama API
	// GeminiAPIKey and AnthropicAPIKey read their provider's API key. New
	// calls only the one the configured provider needs, so a key that is not
	// used is never read (on darwin, reading one can raise a Keychain prompt).
	// A nil reader is a key that is not set.
	GeminiAPIKey    func() (string, error)
	AnthropicAPIKey func() (string, error)
	BatchTokens     int // approximate number of tokens per batch for summarization
}

// providerFor builds the provider cfg names, reading the one setting that
// provider cannot run without.
func (c Config) providerFor() (func(*mcp.ServerSession) Provider, error) {
	switch c.Provider {
	case ProviderSampling:
		return func(session *mcp.ServerSession) Provider { return NewSamplingProvider(session) }, nil
	case ProviderGemini:
		key, err := requiredKey(c.GeminiAPIKey, "MCP_SUMMARIZE_GEMINI_API_KEY", c.Provider)
		if err != nil {
			return nil, err
		}
		return fixedProvider(NewGeminiProvider(key, c.Model)), nil
	case ProviderOllama:
		if c.OllamaURL == "" {
			return nil, fmt.Errorf("MCP_SUMMARIZE_OLLAMA_URL is required when using --summarize-provider=ollama")
		}
		return fixedProvider(NewOllamaProvider(c.OllamaURL, c.Model)), nil
	case ProviderAnthropic:
		key, err := requiredKey(c.AnthropicAPIKey, "MCP_SUMMARIZE_ANTHROPIC_API_KEY", c.Provider)
		if err != nil {
			return nil, err
		}
		return fixedProvider(NewAnthropicProvider(key, c.Model)), nil
	default:
		return nil, fmt.Errorf("invalid summarization provider %q (must be 'sampling', 'ollama', 'gemini', or 'anthropic')", c.Provider)
	}
}

// requiredKey reads provider's API key through read, telling a key that is
// not set (a nil read among them) apart from one that is stored but could
// not be read.
func requiredKey(read func() (string, error), env string, provider ProviderName) (string, error) {
	var key string
	if read != nil {
		var err error
		if key, err = read(); err != nil {
			return "", fmt.Errorf("the %s API key is unreadable (%w)", provider, err)
		}
	}
	if key == "" {
		return "", fmt.Errorf("the %s API key is not set: --summarize-provider=%s needs %s, or the key stored with `mcp-telegram config set %s <key>`", provider, provider, env, provider)
	}
	return key, nil
}

// fixedProvider serves p to every tool call regardless of its session.
func fixedProvider(p Provider) func(*mcp.ServerSession) Provider {
	return func(*mcp.ServerSession) Provider { return p }
}

// errBodySnippetMax bounds how many bytes of an HTTP error response body are
// included in returned errors. Provider APIs sometimes return large HTML
// error pages (proxy 5xx, nginx defaults) — without a cap those would bloat
// MCP tool output and crowd out useful context. 1 KiB is enough to keep a
// JSON error payload intact while cutting HTML floods to a readable fragment.
const errBodySnippetMax = 1024

// errBodySnippet renders a response body as a safe-to-log string, truncating
// at errBodySnippetMax bytes with an explicit "(truncated N bytes)" marker so
// operators can tell when they're seeing a partial response.
func errBodySnippet(body []byte) string {
	if len(body) <= errBodySnippetMax {
		return string(body)
	}
	return fmt.Sprintf("%s... (truncated %d bytes)", body[:errBodySnippetMax], len(body)-errBodySnippetMax)
}

// httpMaxResponseBytes bounds how much of a provider response body we read into
// memory. Provider APIs occasionally return large HTML error pages; the limit
// keeps a pathological response from bloating memory.
const httpMaxResponseBytes = 10 << 20

// postJSON marshals reqBody as JSON, POSTs it to url with Content-Type plus the
// given headers, and unmarshals a 200 response into respBody. It is the shared
// HTTP pipeline for the direct-LLM providers (anthropic/gemini/ollama), so
// timeout-independent behaviour (status handling, body limits, error snippets)
// lives in one place. providerName tags errors for attribution. A non-200
// status becomes an error carrying a bounded snippet of the body; the
// provider-specific inline-error check (some APIs report errors with HTTP 200)
// stays in the caller, which inspects respBody after this returns nil.
func postJSON(ctx context.Context, client *http.Client, providerName, url string, headers map[string]string, reqBody, respBody any) error {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("creating request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("sending request: %w", err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, httpMaxResponseBytes))
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Debug("summarize: response body close failed", "provider", providerName, "err", closeErr)
		}
		if readErr != nil {
			return fmt.Errorf("reading response (status %d): %w", resp.StatusCode, readErr)
		}

		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(raw, respBody); err != nil {
				return fmt.Errorf("unmarshaling response: %w", err)
			}
			return nil
		}

		statusErr := fmt.Errorf("%s returned status %d: %s", providerName, resp.StatusCode, errBodySnippet(raw))
		if attempt == 3 || !retryableProviderStatus(resp.StatusCode) {
			return statusErr
		}
		if err := waitForRetry(ctx, retryDelay(resp.Header.Get("Retry-After"), attempt)); err != nil {
			return fmt.Errorf("%w: %w", err, statusErr)
		}
	}
	panic("unreachable")
}

func retryableProviderStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusInternalServerError || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func retryDelay(retryAfter string, attempt int) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(retryAfter); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	base := 200 * time.Millisecond * time.Duration(1<<(attempt-1))
	// Small bounded jitter prevents synchronised retries without introducing a
	// shared pseudo-random generator into request handling.
	return base + time.Duration(time.Now().UnixNano()%int64(100*time.Millisecond))
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
