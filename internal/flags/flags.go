package flags

import (
	"context"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/tolmachov/mcp-telegram/internal/summarize"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
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
	FloodWaitMaxSecs     = "flood-wait-max-seconds"
	Variant              = "variant"
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

// APIIDFlag defines --api-id. It is intentionally NOT marked Required so that
// `mcp-telegram run` can still start the MCP stdio transport when credentials
// are absent and surface a JSON-RPC init error through the protocol.
// `login`/`logout` re-check for a non-zero value in their Action closures.
func APIIDFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:    APIID,
		Usage:   "Telegram API ID (optional for 'run'; required for 'login'/'logout')",
		Sources: cli.EnvVars(EnvTelegramAPIID),
	}
}

// APIHashFlag defines --api-hash. See APIIDFlag for why Required is omitted.
func APIHashFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    APIHash,
		Usage:   "Telegram API Hash (optional for 'run'; required for 'login'/'logout')",
		Sources: cli.EnvVars(EnvTelegramAPIHash),
	}
}

// AllowedPathsFlag defines --allowed-paths. It intentionally sets no default
// Value: computing the default backup directory touches the filesystem, which
// must not happen while merely constructing flags (e.g. for `run --help`). When
// the flag and its env var are both empty, the run command fills in the default
// lazily via tools.DefaultBackupDir (see app.go).
func AllowedPathsFlag() *cli.StringSliceFlag {
	return &cli.StringSliceFlag{
		Name:    AllowedPaths,
		Usage:   "Allowed directories for file operations (defaults to the OS backup directory when unset)",
		Sources: cli.EnvVars("TELEGRAM_ALLOWED_PATHS"),
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

// FloodWaitMaxSecsFlag defines --flood-wait-max-seconds: how long the client
// will wait out a Telegram FLOOD_WAIT before failing with a retry-after error.
func FloodWaitMaxSecsFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:    FloodWaitMaxSecs,
		Value:   int(tgclient.DefaultFloodWaitMaxWait / time.Second),
		Usage:   "Maximum seconds to wait out a Telegram FLOOD_WAIT before failing fast with a retry-after hint. Keep it below your MCP client's tool-call timeout (Claude Desktop cancels at ~240s) — waiting longer just makes the client time out instead. Raise only for headless/automation runs with no such timeout.",
		Sources: cli.EnvVars("TELEGRAM_FLOOD_WAIT_MAX_SECONDS"),
	}
}

// VariantFlag selects a single SEP-2053 server variant to expose. Empty (the
// default) exposes all variants and lets the client pick via hints; clients
// that don't support variants get the full set. Set it to pin one variant for
// a client that can't negotiate (e.g. --variant research for a read-only bot).
// The value is validated in server.New.
func VariantFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    Variant,
		Usage:   "Expose only one server variant: 'full' (all tools), 'compact' (all tools, short descriptions), or 'research' (read-only subset). Empty exposes all three and lets the client choose.",
		Sources: cli.EnvVars("TELEGRAM_VARIANT"),
	}
}
