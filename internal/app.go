package internal

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/config"
	"github.com/tolmachov/mcp-telegram/internal/flags"
	"github.com/tolmachov/mcp-telegram/internal/server"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/summarize"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tools"
)

// Version contains semantic version number of application.
var Version = "dev"

const serviceName = "mcp-telegram"

// errMissingCredentials is what login and logout report when the API
// credentials are unset. Unlike `run`, they are interactive and must report it
// directly instead of deferring to the MCP peer (see Server.Run).
var errMissingCredentials = fmt.Errorf("%s and %s are required (set via env, flags, or 'config set')", flags.EnvTelegramAPIID, flags.EnvTelegramAPIHash)

// resolveCredentials builds the Telegram client Config for cmd: the API ID and
// hash from their flags or the secure config store, and the flood-wait ceiling
// from its flag. Unset credentials are left zero for the caller to judge.
func resolveCredentials(cmd *cli.Command) (*tgclient.Config, error) {
	resolver, err := config.NewResolver(cmd)
	if err != nil {
		return nil, err
	}
	apiID, err := resolver.Int(flags.APIID, flags.EnvTelegramAPIID)
	if err != nil {
		return nil, err
	}
	apiHash, err := resolver.String(flags.APIHash, flags.EnvTelegramAPIHash)
	if err != nil {
		return nil, err
	}
	return &tgclient.Config{
		APIID:            apiID,
		APIHash:          apiHash,
		FloodWaitMaxWait: time.Duration(cmd.Int(flags.FloodWaitMaxSecs)) * time.Second,
	}, nil
}

// buildAuthOptions assembles the mandatory OAuth configuration for HTTP.
func buildAuthOptions(ctx context.Context, cmd *cli.Command) (*authsrv.Config, sessionstore.Store, error) {
	allow, err := authsrv.ParseAllowlist(cmd.StringSlice(flags.AuthAllowedUsers))
	if err != nil {
		return nil, nil, fmt.Errorf("--%s: %w", flags.AuthAllowedUsers, err)
	}

	cfg := &authsrv.Config{
		IssuerURL:        cmd.String(flags.AuthIssuerURL),
		Allow:            allow,
		TokenKeys:        cmd.StringSlice(flags.AuthTokenKey),
		ExtraRedirects:   cmd.StringSlice(flags.AuthAllowedRedirects),
		TrustedProxyHops: cmd.Int(flags.AuthTrustedProxyHops),
	}
	normalized := cfg.Normalized()
	cfg = &normalized
	if err := cfg.Validate(); err != nil {
		return nil, nil, fmt.Errorf("auth configuration: %w", err)
	}

	bucket := cmd.String(flags.AuthSessionBucket)
	dir := cmd.String(flags.AuthSessionDir)
	var backend sessionstore.Store
	switch {
	case bucket != "" && dir != "":
		return nil, nil, fmt.Errorf("--%s and --%s are mutually exclusive", flags.AuthSessionBucket, flags.AuthSessionDir)
	case bucket != "":
		gcs, err := sessionstore.NewGCS(ctx, bucket)
		if err != nil {
			return nil, nil, err
		}
		backend = gcs
	case dir != "":
		fs, err := sessionstore.NewFS(dir)
		if err != nil {
			return nil, nil, err
		}
		backend = fs
	default:
		return nil, nil, fmt.Errorf("HTTP requires exactly one of --%s / --%s", flags.AuthSessionBucket, flags.AuthSessionDir)
	}

	cipher, err := sessionstore.NewCipher(cfg.TokenKeys, cfg.IssuerURL)
	if err != nil {
		return nil, nil, err
	}
	return cfg, sessionstore.Encrypted(backend, cipher), nil
}

// New creates a new instance of application.
func New(in io.Reader, out, errOut io.Writer) *cli.Command {
	return &cli.Command{
		Name:      serviceName,
		Version:   Version,
		Usage:     "MCP server for Telegram integration",
		Reader:    in,
		Writer:    out,
		ErrWriter: errOut,
		Commands: []*cli.Command{
			{
				Name:  "run",
				Usage: "Run the MCP server",
				Flags: []cli.Flag{
					flags.APIIDFlag(),
					flags.APIHashFlag(),
					flags.AllowedPathsFlag(),
					flags.SummarizeProviderFlag(),
					flags.SummarizeModelFlag(),
					flags.OllamaURLFlag(),
					flags.GeminiAPIKeyFlag(),
					flags.AnthropicAPIKeyFlag(),
					flags.SummarizeBatchTokensFlag(),
					flags.MediaMaxBytesFlag(),
					flags.TGRateLimitRPSFlag(),
					flags.PinnedRefreshSecsFlag(),
					flags.FloodWaitMaxSecsFlag(),
					flags.VariantFlag(),
					flags.TransportFlag(),
					flags.HTTPAddrFlag(),
					flags.LogFormatFlag(),
					flags.LogLevelFlag(),
					flags.AuthIssuerURLFlag(),
					flags.AuthAllowedUsersFlag(),
					flags.AuthTokenKeyFlag(),
					flags.AuthAllowedRedirectsFlag(),
					flags.AuthSessionBucketFlag(),
					flags.AuthSessionDirFlag(),
					flags.AuthTrustedProxyHopsFlag(),
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					// Credential validation is intentionally NOT performed here.
					// Server.Run handles missing/invalid credentials itself: over
					// stdio it serves login-required mode, which names the problem
					// in the server's own instructions and tool list. Exiting the
					// process here would just look like "Server disconnected".
					// login/logout still pre-flight-validate because they are
					// interactive commands without an MCP peer to report to.
					cfg, err := resolveCredentials(cmd)
					if err != nil {
						return err
					}
					resolver, err := config.NewResolver(cmd)
					if err != nil {
						return err
					}
					var geminiKey, anthropicKey string
					summarizeProvider := summarize.ProviderName(cmd.String(flags.SummarizeProvider))
					if summarizeProvider == summarize.ProviderGemini {
						geminiKey, err = resolver.String(flags.GeminiAPIKey, flags.EnvGeminiAPIKey)
						if err != nil {
							return err
						}
					}
					if summarizeProvider == summarize.ProviderAnthropic {
						anthropicKey, err = resolver.String(flags.AnthropicAPIKey, flags.EnvAnthropicAPIKey)
						if err != nil {
							return err
						}
					}
					transport := cmd.String(flags.Transport)
					variant := cmd.String(flags.Variant)
					allowedPaths := cmd.StringSlice(flags.AllowedPaths)
					backupEnabled := transport != server.TransportHTTP && variant != server.VariantResearch
					if !backupEnabled {
						// HTTP and research never expose BackupMessages, so their startup
						// must not inspect or create any backup directory either.
						allowedPaths = nil
					} else if len(allowedPaths) == 0 {
						// No flag/env value: fall back to the OS backup directory,
						// computed here (lazily) rather than at flag construction so
						// help/version never touch the filesystem. If it can't be
						// determined, leave the list empty — BackupMessages then
						// reports "no allowed paths configured" with guidance.
						if d, err := tools.DefaultBackupDir(); err == nil {
							allowedPaths = []string{d}
						} else {
							slog.Warn("could not determine default backup directory; "+
								"BackupMessages will require --allowed-paths", "err", err)
						}
					}
					summarizeCfg := summarize.Config{
						Provider:        summarizeProvider,
						Model:           cmd.String(flags.SummarizeModel),
						OllamaURL:       cmd.String(flags.OllamaURL),
						GeminiAPIKey:    geminiKey,
						AnthropicAPIKey: anthropicKey,
						BatchTokens:     cmd.Int(flags.SummarizeBatchTokens),
					}
					var authCfg *authsrv.Config
					var store sessionstore.Store
					if transport == server.TransportHTTP {
						authCfg, store, err = buildAuthOptions(ctx, cmd)
						if err != nil {
							return err
						}
					}
					httpAddr, err := config.ResolveHTTPAddr(cmd.String(flags.HTTPAddr), cmd.IsSet(flags.HTTPAddr), os.LookupEnv)
					if err != nil {
						return err
					}
					serverOpts := server.Options{
						Config:         cfg,
						Version:        Version,
						Auth:           authCfg,
						SessionStore:   store,
						AllowedPaths:   allowedPaths,
						SummarizeCfg:   summarizeCfg,
						MediaMaxBytes:  cmd.Int(flags.MediaMaxBytes),
						TGRateLimitRPS: cmd.Int(flags.TGRateLimitRPS),
						PinnedRefresh:  time.Duration(cmd.Int(flags.PinnedRefreshSecs)) * time.Second,
						Variant:        variant,
						Transport:      transport,
						HTTPAddr:       httpAddr,
						LogFormat:      cmd.String(flags.LogFormat),
						LogLevel:       cmd.String(flags.LogLevel),
						Stdin:          cmd.Root().Reader,
						Stdout:         cmd.Root().Writer,
						ErrOut:         cmd.Root().ErrWriter,
					}
					srv, err := server.New(serverOpts)
					if err != nil {
						return err
					}
					return srv.Run(ctx)
				},
			},
			{
				Name:  "login",
				Usage: "Login to Telegram",
				Flags: []cli.Flag{
					flags.APIIDFlag(),
					flags.APIHashFlag(),
					flags.PhoneFlag(),
					flags.FloodWaitMaxSecsFlag(),
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					// Phone is enforced by PhoneFlag(Required).
					cfg, err := resolveCredentials(cmd)
					if err != nil {
						return err
					}
					if cfg.APIID == 0 || cfg.APIHash == "" {
						return errMissingCredentials
					}
					return tgclient.Login(ctx, cfg, cmd.String(flags.Phone), cmd.Root().Reader, cmd.Root().Writer)
				},
			},
			{
				Name:  "logout",
				Usage: "Logout from Telegram",
				Flags: []cli.Flag{
					flags.APIIDFlag(),
					flags.APIHashFlag(),
					flags.FloodWaitMaxSecsFlag(),
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					cfg, err := resolveCredentials(cmd)
					if err != nil {
						return err
					}
					if cfg.APIID == 0 || cfg.APIHash == "" {
						return errMissingCredentials
					}
					return tgclient.Logout(ctx, cfg, cmd.Root().Writer)
				},
			},
			{
				Name:  "config",
				Usage: "Manage secure configuration values",
				Commands: []*cli.Command{
					{
						Name:      "set",
						Usage:     "Store a config value securely (keys: api-id, api-hash, anthropic, gemini)",
						ArgsUsage: "<key> <value>",
						Action:    configSetAction,
					},
					{
						Name:   "list",
						Usage:  "List stored config keys",
						Action: configListAction,
					},
					{
						Name:      "delete",
						Usage:     "Remove a stored config value",
						ArgsUsage: "<key>",
						Action:    configDeleteAction,
					},
				},
			},
		},
	}
}
