package internal

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
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

// requireCredentials builds a Telegram Config from the shared api-id/api-hash
// flags and fails if either is unset. login and logout both call it: unlike
// `run`, they are interactive and must report missing credentials directly
// instead of deferring to a JSON-RPC init error.
func requireCredentials(cmd *cli.Command) (*tgclient.Config, error) {
	cfg := &tgclient.Config{
		APIID:   cmd.Int(flags.APIID),
		APIHash: cmd.String(flags.APIHash),
	}
	if cfg.APIID == 0 || cfg.APIHash == "" {
		return nil, fmt.Errorf("%s and %s are required (set via env, flags, or 'config set')", flags.EnvTelegramAPIID, flags.EnvTelegramAPIHash)
	}
	return cfg, nil
}

// buildAuthOptions assembles the auth config and per-user session store for
// --auth telegram, or returns (nil, nil, nil) for --auth none. Fails fast on
// misconfiguration: unlike missing Telegram credentials (reported through the
// MCP init-error dance for stdio hosts), auth is HTTP-only and has no MCP
// peer yet, so a plain error is the right surface.
func buildAuthOptions(ctx context.Context, cmd *cli.Command) (*authsrv.Config, sessionstore.Store, error) {
	if cmd.String(flags.Auth) != flags.AuthModeTelegram {
		return nil, nil, nil
	}

	allow, err := authsrv.ParseAllowlist(cmd.StringSlice(flags.AuthAllowedUsers))
	if err != nil {
		return nil, nil, fmt.Errorf("--%s: %w", flags.AuthAllowedUsers, err)
	}

	cfg := &authsrv.Config{
		IssuerURL:      cmd.String(flags.AuthIssuerURL),
		Allow:          allow,
		TokenKeys:      cmd.StringSlice(flags.AuthTokenKey),
		ExtraRedirects: cmd.StringSlice(flags.AuthAllowedRedirects),
	}
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
		return nil, nil, fmt.Errorf("--auth telegram requires exactly one of --%s / --%s", flags.AuthSessionBucket, flags.AuthSessionDir)
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
					flags.AuthFlag(),
					flags.AuthIssuerURLFlag(),
					flags.AuthAllowedUsersFlag(),
					flags.AuthTokenKeyFlag(),
					flags.AuthAllowedRedirectsFlag(),
					flags.AuthSessionBucketFlag(),
					flags.AuthSessionDirFlag(),
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					// Credential validation is intentionally NOT performed here.
					// Server.Run handles missing/invalid credentials by writing
					// a JSON-RPC error on the initialize request, which is what
					// MCP hosts like Claude Desktop render to the user. Exiting
					// the process here would just look like "Server disconnected".
					// login/logout still pre-flight-validate because they are
					// interactive commands without an MCP peer to report to.
					cfg := &tgclient.Config{
						APIID:            cmd.Int(flags.APIID),
						APIHash:          cmd.String(flags.APIHash),
						FloodWaitMaxWait: time.Duration(cmd.Int(flags.FloodWaitMaxSecs)) * time.Second,
					}
					allowedPaths := cmd.StringSlice(flags.AllowedPaths)
					if len(allowedPaths) == 0 {
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
						Provider:        summarize.ProviderName(cmd.String(flags.SummarizeProvider)),
						Model:           cmd.String(flags.SummarizeModel),
						OllamaURL:       cmd.String(flags.OllamaURL),
						GeminiAPIKey:    cmd.String(flags.GeminiAPIKey),
						AnthropicAPIKey: cmd.String(flags.AnthropicAPIKey),
						BatchTokens:     cmd.Int(flags.SummarizeBatchTokens),
					}
					authCfg, store, err := buildAuthOptions(ctx, cmd)
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
						Variant:        cmd.String(flags.Variant),
						Transport:      cmd.String(flags.Transport),
						HTTPAddr:       cmd.String(flags.HTTPAddr),
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
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					// Phone is enforced by PhoneFlag(Required); credentials are
					// pre-flight-validated here because login is interactive and
					// has no MCP peer to report an init error to.
					cfg, err := requireCredentials(cmd)
					if err != nil {
						return err
					}
					return tgclient.Login(ctx, cfg, cmd.String(flags.Phone))
				},
			},
			{
				Name:  "logout",
				Usage: "Logout from Telegram",
				Flags: []cli.Flag{
					flags.APIIDFlag(),
					flags.APIHashFlag(),
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					cfg, err := requireCredentials(cmd)
					if err != nil {
						return err
					}
					return tgclient.Logout(ctx, cfg)
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
