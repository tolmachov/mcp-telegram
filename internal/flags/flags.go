package flags

import (
	"context"
	"log/slog"

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
	MediaMaxBytes        = "media-max-bytes"
	TGRateLimitRPS       = "tg-rate-limit-rps"
	PinnedRefreshSecs    = "pinned-refresh-seconds"
)

// DefaultPinnedRefreshSeconds is the default polling interval for the
// pinned-chat background watcher. 30s balances freshness against API load;
// the previous SDK supported on-demand refresh, but the official Go SDK
// has no BeforeListResources hook.
const DefaultPinnedRefreshSeconds = 30

// DefaultMediaMaxBytes is the default cap on a single GetMedia download.
// 50 MiB is large enough for any practical photo (Telegram's photo limit is
// 10 MiB) and any reasonable thumbnail of a video, while being small enough
// to keep base64-encoded responses inside MCP context-window economics.
const DefaultMediaMaxBytes = 50 * 1024 * 1024

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
	var defaultDirs []string
	if d, err := tools.DefaultBackupDir(); err != nil {
		slog.Warn("could not determine default backup directory; --allowed-paths will have no default", "err", err)
	} else {
		defaultDirs = []string{d}
	}
	return &cli.StringSliceFlag{
		Name:    AllowedPaths,
		Usage:   "Allowed directories for file operations",
		Sources: cli.EnvVars("TELEGRAM_ALLOWED_PATHS"),
		Value:   defaultDirs,
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

func MediaMaxBytesFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:    MediaMaxBytes,
		Value:   DefaultMediaMaxBytes,
		Usage:   "Maximum bytes that GetMedia will download in a single call (cap to avoid OOM on huge attachments)",
		Sources: cli.EnvVars("TELEGRAM_MEDIA_MAX_BYTES"),
	}
}

func TGRateLimitRPSFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:    TGRateLimitRPS,
		Value:   0, // 0 → use messages.DefaultRateLimitRPS at provider construction
		Usage:   "Requests-per-second ceiling for history-fetching calls to Telegram. 0 uses the safe default. Raise with care: exceeding Telegram's FLOOD_WAIT thresholds will pause all tools.",
		Sources: cli.EnvVars("TELEGRAM_RATE_LIMIT_RPS"),
	}
}

func PinnedRefreshSecsFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:    PinnedRefreshSecs,
		Value:   DefaultPinnedRefreshSeconds,
		Usage:   "Polling interval (seconds) for the pinned-chat resource watcher. 0 disables the watcher entirely.",
		Sources: cli.EnvVars("TELEGRAM_PINNED_REFRESH_SECONDS"),
	}
}
