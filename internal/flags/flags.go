package flags

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/summarize"
)

// Environment variable name constants.
const (
	EnvTelegramAPIID   = "MCP_TELEGRAM_API_ID"
	EnvTelegramAPIHash = "MCP_TELEGRAM_API_HASH"
	EnvAnthropicAPIKey = "MCP_SUMMARIZE_ANTHROPIC_API_KEY" //nolint:gosec // env var name, not a credential
	EnvGeminiAPIKey    = "MCP_SUMMARIZE_GEMINI_API_KEY"    //nolint:gosec // env var name, not a credential
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
	Transport            = "transport"
	HTTPAddr             = "http-addr"
	LogFormat            = "log-format"
	LogLevel             = "log-level"
	AuthIssuerURL        = "auth-issuer-url"
	AuthAllowedUsers     = "auth-allowed-users"
	AuthTokenKey         = "auth-token-key" //nolint:gosec // flag name, not a credential
	AuthAllowedRedirects = "auth-allowed-redirects"
	AuthSessionBucket    = "auth-session-bucket"
	AuthSessionDir       = "auth-session-dir"
	AuthTrustedProxyHops = "auth-trusted-proxy-hops"
)

// DefaultPinnedRefreshSeconds is the default polling interval for the
// pinned-chat background watcher. 30s balances freshness against API load;
// the previous SDK supported on-demand refresh, but the official Go SDK
// has no BeforeListResources hook.
const DefaultPinnedRefreshSeconds = 30

// DefaultSummarizeBatchTokens is the default approximate number of tokens per
// summarisation batch.
const DefaultSummarizeBatchTokens = 8000

// DefaultRateLimitRPS is the default request rate (requests per second) for
// history-fetching calls. Kept conservative to avoid tripping Telegram's
// flood-wait on bursty tools like BackupMessages.
const DefaultRateLimitRPS = 1

// DefaultFloodWaitMaxSeconds is the default ceiling for auto-waiting out a
// Telegram FLOOD_WAIT. It MUST stay well below the MCP client's tool-call
// timeout (Claude Desktop cancels at ~240s): auto-waiting longer is pointless
// because the client cancels the call first, turning a recoverable rate limit
// into a generic "no result received" timeout. So short, transient waits are
// absorbed transparently, while anything longer fails fast and every tool
// renders it as an actionable retry-after message (see tools.floodWaitMessage),
// beating the client's hang-until-timeout.
const DefaultFloodWaitMaxSeconds = 60

// DefaultMediaMaxBytes is the default cap on a single GetMedia download.
// 50 MiB is large enough for any practical photo (Telegram's photo limit is
// 10 MiB) and any reasonable thumbnail of a video, while being small enough
// to keep base64-encoded responses inside MCP context-window economics.
const DefaultMediaMaxBytes = 50 * 1024 * 1024

// requirePositive rejects a zero or negative value for an int flag whose
// consumer has no meaning for it.
func requirePositive(name string) func(context.Context, *cli.Command, int) error {
	return func(_ context.Context, _ *cli.Command, value int) error {
		if value <= 0 {
			return fmt.Errorf("--%s must be positive", name)
		}
		return nil
	}
}

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
		Sources: cli.EnvVars("MCP_TELEGRAM_ALLOWED_PATHS"),
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
		Sources: cli.EnvVars("MCP_SUMMARIZE_PROVIDER"),
		Action: func(_ context.Context, _ *cli.Command, value string) error {
			return summarize.ValidateProviderName(value)
		},
	}
}

func SummarizeModelFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    SummarizeModel,
		Usage:   "Model for summarization (provider-specific)",
		Sources: cli.EnvVars("MCP_SUMMARIZE_MODEL"),
	}
}

func OllamaURLFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    OllamaURL,
		Value:   "http://localhost:11434",
		Usage:   "Ollama API URL (used when summarize-provider is 'ollama')",
		Sources: cli.EnvVars("MCP_SUMMARIZE_OLLAMA_URL"),
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
		Value:   DefaultSummarizeBatchTokens,
		Usage:   "Approximate number of tokens per batch for summarization",
		Sources: cli.EnvVars("MCP_SUMMARIZE_BATCH_TOKENS"),
		Action:  requirePositive(SummarizeBatchTokens),
	}
}

func MediaMaxBytesFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:    MediaMaxBytes,
		Value:   DefaultMediaMaxBytes,
		Usage:   "Maximum bytes that GetMedia will download in a single call (cap to avoid OOM on huge attachments)",
		Sources: cli.EnvVars("MCP_TELEGRAM_MEDIA_MAX_BYTES"),
	}
}

func TGRateLimitRPSFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:    TGRateLimitRPS,
		Value:   DefaultRateLimitRPS,
		Usage:   "Requests-per-second ceiling for history-fetching calls to Telegram. Raise with care: exceeding Telegram's FLOOD_WAIT thresholds will pause all tools.",
		Sources: cli.EnvVars("MCP_TELEGRAM_RATE_LIMIT_RPS"),
		Action:  requirePositive(TGRateLimitRPS),
	}
}

func PinnedRefreshSecsFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:    PinnedRefreshSecs,
		Value:   DefaultPinnedRefreshSeconds,
		Usage:   "Polling interval (seconds) for the pinned-chat resource watcher. 0 disables the watcher entirely.",
		Sources: cli.EnvVars("MCP_TELEGRAM_PINNED_REFRESH_SECONDS"),
	}
}

// FloodWaitMaxSecsFlag defines --flood-wait-max-seconds: how long the client
// will wait out a Telegram FLOOD_WAIT before failing with a retry-after error.
func FloodWaitMaxSecsFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:    FloodWaitMaxSecs,
		Value:   DefaultFloodWaitMaxSeconds,
		Usage:   "Maximum seconds to wait out a Telegram FLOOD_WAIT before failing fast with a retry-after hint. Keep it below your MCP client's tool-call timeout (Claude Desktop cancels at ~240s) — waiting longer just makes the client time out instead. Raise only for headless/automation runs with no such timeout.",
		Sources: cli.EnvVars("MCP_TELEGRAM_FLOOD_WAIT_MAX_SECONDS"),
		Action:  requirePositive(FloodWaitMaxSecs),
	}
}

// TransportFlag selects the MCP transport: newline-delimited stdio (default,
// what desktop hosts spawn) or streamable HTTP (for remote deployments).
// The value is validated in server.New.
func TransportFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    Transport,
		Value:   "stdio",
		Usage:   "MCP transport: 'stdio' (default) or 'http' (streamable HTTP on --http-addr)",
		Sources: cli.EnvVars("MCP_TRANSPORT"),
	}
}

// HTTPAddrFlag defines the listen address used when --transport is 'http'.
func HTTPAddrFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    HTTPAddr,
		Value:   "127.0.0.1:8080",
		Usage:   "Listen address for the streamable HTTP transport (used with --transport http)",
		Sources: cli.EnvVars("MCP_HTTP_ADDR"),
	}
}

func AuthIssuerURLFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    AuthIssuerURL,
		Usage:   "Public base URL of this server (OAuth issuer). Required for the HTTP transport.",
		Sources: cli.EnvVars("MCP_AUTH_ISSUER_URL"),
	}
}

func AuthAllowedUsersFlag() *cli.StringSliceFlag {
	return &cli.StringSliceFlag{
		Name:    AuthAllowedUsers,
		Usage:   "Telegram user ids allowed to log in (comma-separated), or '*' alone to allow any account. Required for HTTP.",
		Sources: cli.EnvVars("MCP_AUTH_ALLOWED_USERS"),
		// Validate the wildcard/ids rule at parse time so a bad value fails
		// fast with a clear message, using the same parser server startup does.
		Action: func(_ context.Context, _ *cli.Command, value []string) error {
			if _, err := authsrv.ParseAllowlist(value); err != nil {
				return fmt.Errorf("--%s: %w", AuthAllowedUsers, err)
			}
			return nil
		},
	}
}

func AuthTokenKeyFlag() *cli.StringSliceFlag {
	return &cli.StringSliceFlag{
		Name:    AuthTokenKey,
		Usage:   "Base64-encoded 32-byte master keys for tokens and session encryption. Required for HTTP.",
		Sources: cli.EnvVars("MCP_AUTH_TOKEN_KEYS"),
	}
}

func AuthAllowedRedirectsFlag() *cli.StringSliceFlag {
	return &cli.StringSliceFlag{
		Name:    AuthAllowedRedirects,
		Usage:   "Extra exact-match HTTPS redirect URIs allowed for OAuth clients (loopback URIs and the claude.ai/claude.com callbacks are always allowed)",
		Sources: cli.EnvVars("MCP_AUTH_ALLOWED_REDIRECTS"),
	}
}

func AuthSessionBucketFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    AuthSessionBucket,
		Usage:   "GCS bucket for per-user Telegram sessions (exactly one HTTP session backend is required)",
		Sources: cli.EnvVars("MCP_AUTH_SESSION_BUCKET"),
	}
}

func AuthSessionDirFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    AuthSessionDir,
		Usage:   "Local directory for per-user Telegram sessions (exactly one HTTP session backend is required)",
		Sources: cli.EnvVars("MCP_AUTH_SESSION_DIR"),
	}
}

func AuthTrustedProxyHopsFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:    AuthTrustedProxyHops,
		Usage:   "Number of trusted reverse-proxy hops. Zero ignores forwarding headers.",
		Sources: cli.EnvVars("MCP_AUTH_TRUSTED_PROXY_HOPS"),
	}
}

// LogFormatFlag selects the log output format. Empty (the default) resolves
// to JSON in http mode (for GCP Cloud Logging) and text otherwise; see app.go.
func LogFormatFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    LogFormat,
		Usage:   "Log format: 'json' (GCP-structured) or 'text'. Default: json for --transport http, text for stdio.",
		Sources: cli.EnvVars("MCP_LOG_FORMAT"),
	}
}

// LogLevelFlag sets the minimum log level. Debug surfaces the high-volume
// list/lifecycle method calls; info (default) keeps them quiet.
func LogLevelFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    LogLevel,
		Value:   "info",
		Usage:   "Minimum log level: debug, info, warn, or error. Debug also logs list/lifecycle method calls.",
		Sources: cli.EnvVars("MCP_LOG_LEVEL"),
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
		Sources: cli.EnvVars("MCP_VARIANT"),
	}
}
