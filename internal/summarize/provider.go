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

// DefaultBatchTokens is the default number of tokens per batch.
const DefaultBatchTokens = 8000
