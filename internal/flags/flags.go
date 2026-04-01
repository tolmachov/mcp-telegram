package flags

import (
	"context"

	"github.com/urfave/cli/v3"

	"github.com/tolmachov/mcp-telegram/internal/summarize"
	"github.com/tolmachov/mcp-telegram/internal/tools"
)

// Environment variable name constants.
const (
	EnvTelegramAPIID   = "TELEGRAM_API_ID"
	EnvTelegramAPIHash = "TELEGRAM_API_HASH"
	EnvAnthropicAPIKey = "ANTHROPIC_API_KEY" //nolint:gosec // env var name, not a credential
	EnvGeminiAPIKey    = "GEMINI_API_KEY"    //nolint:gosec // env var name, not a credential
)

// Flag name constants.
const (
	APIID                = "api-id"
	APIHash              = "api-hash"
	AllowedPaths         = "allowed-paths"
	Phone                = "phone"
	SummarizeProvider    = "summarize-provider"
	SummarizeModel       = "summarize-model"
	OllamaURL            = "ollama-url"
	GeminiAPIKey         = "gemini-api-key"    //nolint:gosec // flag name, not a credential
	AnthropicAPIKey      = "anthropic-api-key" //nolint:gosec // flag name, not a credential
	SummarizeBatchTokens = "summarize-batch-tokens"
)

func APIIDFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:     APIID,
		Usage:    "Telegram API ID",
		Sources:  cli.EnvVars(EnvTelegramAPIID),
		Required: true,
	}
}

func APIHashFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:     APIHash,
		Usage:    "Telegram API Hash",
		Sources:  cli.EnvVars(EnvTelegramAPIHash),
		Required: true,
	}
}

func AllowedPathsFlag() *cli.StringSliceFlag {
	return &cli.StringSliceFlag{
		Name:    AllowedPaths,
		Usage:   "Allowed directories for file operations",
		Sources: cli.EnvVars("TELEGRAM_ALLOWED_PATHS"),
		Value:   []string{tools.DefaultBackupDir()},
	}
}

func PhoneFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:     Phone,
		Aliases:  []string{"p"},
		Usage:    "Phone number with country code (e.g., +1234567890)",
		Required: true,
	}
}

func SummarizeProviderFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    SummarizeProvider,
		Value:   string(summarize.ProviderSampling),
		Usage:   "Provider for summarization: 'sampling', 'ollama', 'gemini', or 'anthropic'",
		Sources: cli.EnvVars("SUMMARIZE_PROVIDER"),
		Action: func(_ context.Context, _ *cli.Command, value string) error {
			return summarize.ValidateProviderName(value)
		},
	}
}

func SummarizeModelFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    SummarizeModel,
		Usage:   "Model for summarization (provider-specific)",
		Sources: cli.EnvVars("SUMMARIZE_MODEL"),
	}
}

func OllamaURLFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    OllamaURL,
		Value:   "http://localhost:11434",
		Usage:   "Ollama API URL (used when summarize-provider is 'ollama')",
		Sources: cli.EnvVars("OLLAMA_URL"),
	}
}

func GeminiAPIKeyFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    GeminiAPIKey,
		Usage:   "Gemini API key (used when summarize-provider is 'gemini')",
		Sources: cli.EnvVars(EnvGeminiAPIKey),
	}
}

func AnthropicAPIKeyFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    AnthropicAPIKey,
		Usage:   "Anthropic API key (used when summarize-provider is 'anthropic')",
		Sources: cli.EnvVars(EnvAnthropicAPIKey),
	}
}

func SummarizeBatchTokensFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:    SummarizeBatchTokens,
		Value:   summarize.DefaultBatchTokens,
		Usage:   "Approximate number of tokens per batch for summarization",
		Sources: cli.EnvVars("SUMMARIZE_BATCH_TOKENS"),
	}
}
