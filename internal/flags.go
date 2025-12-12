package internal

import (
	"context"

	"github.com/urfave/cli/v3"

	"github.com/tolmachov/mcp-telegram/internal/summarize"
	"github.com/tolmachov/mcp-telegram/internal/tools"
)

const (
	flagAPIID                = "api-id"
	flagAPIHash              = "api-hash"
	flagAllowedPaths         = "allowed-paths"
	flagPhone                = "phone"
	flagSummarizeProvider    = "summarize-provider"
	flagSummarizeModel       = "summarize-model"
	flagOllamaURL            = "ollama-url"
	flagGeminiAPIKey         = "gemini-api-key"    //nolint:gosec // flag name, not a credential
	flagAnthropicAPIKey      = "anthropic-api-key" //nolint:gosec // flag name, not a credential
	flagSummarizeBatchTokens = "summarize-batch-tokens"
)

func apiIDFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:     flagAPIID,
		Usage:    "Telegram API ID",
		Sources:  cli.EnvVars("TELEGRAM_API_ID"),
		Required: true,
	}
}

func apiHashFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:     flagAPIHash,
		Usage:    "Telegram API Hash",
		Sources:  cli.EnvVars("TELEGRAM_API_HASH"),
		Required: true,
	}
}

func allowedPathsFlag() *cli.StringSliceFlag {
	return &cli.StringSliceFlag{
		Name:    flagAllowedPaths,
		Usage:   "Allowed directories for file operations",
		Sources: cli.EnvVars("TELEGRAM_ALLOWED_PATHS"),
		Value:   []string{tools.DefaultBackupDir()},
	}
}

func phoneFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:     flagPhone,
		Aliases:  []string{"p"},
		Usage:    "Phone number with country code (e.g., +1234567890)",
		Required: true,
	}
}

func summarizeProviderFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagSummarizeProvider,
		Value:   string(summarize.ProviderSampling),
		Usage:   "Provider for summarization: 'sampling', 'ollama', 'gemini', or 'anthropic'",
		Sources: cli.EnvVars("SUMMARIZE_PROVIDER"),
		Action: func(_ context.Context, _ *cli.Command, value string) error {
			return summarize.ValidateProviderName(value)
		},
	}
}

func summarizeModelFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagSummarizeModel,
		Usage:   "Model for summarization (provider-specific)",
		Sources: cli.EnvVars("SUMMARIZE_MODEL"),
	}
}

func ollamaURLFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagOllamaURL,
		Value:   "http://localhost:11434",
		Usage:   "Ollama API URL (used when summarize-provider is 'ollama')",
		Sources: cli.EnvVars("OLLAMA_URL"),
	}
}

func geminiAPIKeyFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagGeminiAPIKey,
		Usage:   "Gemini API key (used when summarize-provider is 'gemini')",
		Sources: cli.EnvVars("GEMINI_API_KEY"),
	}
}

func anthropicAPIKeyFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagAnthropicAPIKey,
		Usage:   "Anthropic API key (used when summarize-provider is 'anthropic')",
		Sources: cli.EnvVars("ANTHROPIC_API_KEY"),
	}
}

func summarizeBatchTokensFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:    flagSummarizeBatchTokens,
		Value:   summarize.DefaultBatchTokens,
		Usage:   "Approximate number of tokens per batch for summarization",
		Sources: cli.EnvVars("SUMMARIZE_BATCH_TOKENS"),
	}
}
