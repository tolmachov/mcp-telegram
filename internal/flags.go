package internal

import (
	"context"

	"github.com/urfave/cli/v3"

	"github.com/tolmachov/mcp-telegram/internal/summarize"
	"github.com/tolmachov/mcp-telegram/internal/tools"
)

var (
	apiIDFlag = &cli.IntFlag{
		Name:     "api-id",
		Usage:    "Telegram API ID",
		Sources:  cli.EnvVars("TELEGRAM_API_ID"),
		Required: true,
	}

	apiHashFlag = &cli.StringFlag{
		Name:     "api-hash",
		Usage:    "Telegram API Hash",
		Sources:  cli.EnvVars("TELEGRAM_API_HASH"),
		Required: true,
	}

	allowedPathsFlag = &cli.StringSliceFlag{
		Name:    "allowed-paths",
		Usage:   "Allowed directories for file operations",
		Sources: cli.EnvVars("TELEGRAM_ALLOWED_PATHS"),
		Value:   []string{tools.DefaultBackupDir()},
	}

	phoneFlag = &cli.StringFlag{
		Name:     "phone",
		Aliases:  []string{"p"},
		Usage:    "Phone number with country code (e.g., +1234567890)",
		Required: true,
	}

	summarizeProviderFlag = &cli.StringFlag{
		Name:    "summarize-provider",
		Value:   string(summarize.ProviderSampling),
		Usage:   "Provider for summarization: 'sampling', 'ollama', 'gemini', or 'anthropic'",
		Sources: cli.EnvVars("SUMMARIZE_PROVIDER"),
		Action: func(_ context.Context, _ *cli.Command, value string) error {
			return summarize.ValidateProviderName(value)
		},
	}

	summarizeModelFlag = &cli.StringFlag{
		Name:    "summarize-model",
		Usage:   "Model for summarization (provider-specific)",
		Sources: cli.EnvVars("SUMMARIZE_MODEL"),
	}

	ollamaURLFlag = &cli.StringFlag{
		Name:    "ollama-url",
		Value:   "http://localhost:11434",
		Usage:   "Ollama API URL (used when summarize-provider is 'ollama')",
		Sources: cli.EnvVars("OLLAMA_URL"),
	}

	geminiAPIKeyFlag = &cli.StringFlag{
		Name:    "gemini-api-key",
		Usage:   "Gemini API key (used when summarize-provider is 'gemini')",
		Sources: cli.EnvVars("GEMINI_API_KEY"),
	}

	anthropicAPIKeyFlag = &cli.StringFlag{
		Name:    "anthropic-api-key",
		Usage:   "Anthropic API key (used when summarize-provider is 'anthropic')",
		Sources: cli.EnvVars("ANTHROPIC_API_KEY"),
	}

	summarizeBatchTokensFlag = &cli.IntFlag{
		Name:    "summarize-batch-tokens",
		Value:   summarize.DefaultBatchTokens,
		Usage:   "Approximate number of tokens per batch for summarization",
		Sources: cli.EnvVars("SUMMARIZE_BATCH_TOKENS"),
	}
)
