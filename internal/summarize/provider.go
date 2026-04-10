package summarize

import (
	"context"
	"fmt"
)

// Provider is an interface for LLM providers that can summarize text.
type Provider interface {
	Summarize(ctx context.Context, prompt string) (string, error)
}

// ProviderName represents a valid summarization provider name.
type ProviderName string

const (
	ProviderSampling  ProviderName = "sampling"
	ProviderOllama    ProviderName = "ollama"
	ProviderGemini    ProviderName = "gemini"
	ProviderAnthropic ProviderName = "anthropic"
)

// ValidateProviderName checks if the provider name is valid.
func ValidateProviderName(name string) error {
	switch ProviderName(name) {
	case ProviderSampling, ProviderOllama, ProviderGemini, ProviderAnthropic:
		return nil
	default:
		return fmt.Errorf("invalid provider: %q (must be 'sampling', 'ollama', 'gemini', or 'anthropic')", name)
	}
}

// Config holds configuration for summarization providers.
type Config struct {
	Provider        ProviderName // "sampling", "ollama", "gemini", or "anthropic"
	Model           string       // provider-specific model name
	OllamaURL       string       // URL for Ollama API
	GeminiAPIKey    string       // API key for Gemini
	AnthropicAPIKey string       // API key for Anthropic
	BatchTokens     int          // approximate number of tokens per batch for summarization
}

// String implements fmt.Stringer so accidental logging of a Config value
// (via %v / %s) never leaks API keys. GoString provides the same guarantee
// for %#v. We only report whether each key is set, never its contents.
func (c Config) String() string {
	return fmt.Sprintf(
		"summarize.Config{Provider:%q Model:%q OllamaURL:%q GeminiAPIKey:%s AnthropicAPIKey:%s BatchTokens:%d}",
		c.Provider, c.Model, c.OllamaURL,
		redactedPresence(c.GeminiAPIKey), redactedPresence(c.AnthropicAPIKey),
		c.BatchTokens,
	)
}

// GoString implements fmt.GoStringer so %#v is also redacted.
func (c Config) GoString() string { return c.String() }

func redactedPresence(s string) string {
	if s == "" {
		return "<unset>"
	}
	return "<redacted>"
}

// DefaultBatchTokens is the default number of tokens per batch.
const DefaultBatchTokens = 8000

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
