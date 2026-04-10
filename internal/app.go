package internal

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/tolmachov/mcp-telegram/internal/flags"
	"github.com/tolmachov/mcp-telegram/internal/server"
	"github.com/tolmachov/mcp-telegram/internal/summarize"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// Version contains semantic version number of application.
var Version = "dev"

const serviceName = "mcp-telegram"

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
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					cfg := &tgclient.Config{
						APIID:   cmd.Int(flags.APIID),
						APIHash: cmd.String(flags.APIHash),
					}
					if cfg.APIID == 0 || cfg.APIHash == "" {
						return fmt.Errorf("%s and %s are required (set via env, flags, or 'config set')", flags.EnvTelegramAPIID, flags.EnvTelegramAPIHash)
					}
					allowedPaths := cmd.StringSlice(flags.AllowedPaths)
					summarizeCfg := summarize.Config{
						Provider:        summarize.ProviderName(cmd.String(flags.SummarizeProvider)),
						Model:           cmd.String(flags.SummarizeModel),
						OllamaURL:       cmd.String(flags.OllamaURL),
						GeminiAPIKey:    cmd.String(flags.GeminiAPIKey),
						AnthropicAPIKey: cmd.String(flags.AnthropicAPIKey),
						BatchTokens:     cmd.Int(flags.SummarizeBatchTokens),
					}
					serverOpts := server.Options{
						Config:           cfg,
						Version:          Version,
						AllowedPaths:     allowedPaths,
						SummarizeCfg:     summarizeCfg,
						MediaMaxBytes:    cmd.Int(flags.MediaMaxBytes),
						TGRateLimitRPS:   cmd.Int(flags.TGRateLimitRPS),
						PinnedRefresh:    time.Duration(cmd.Int(flags.PinnedRefreshSecs)) * time.Second,
						Stdin:            cmd.Root().Reader,
						Stdout:           cmd.Root().Writer,
						ErrOut:           cmd.Root().ErrWriter,
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
					phone := cmd.String(flags.Phone)
					if phone == "" {
						return fmt.Errorf("phone number is required")
					}
					cfg := &tgclient.Config{
						APIID:   cmd.Int(flags.APIID),
						APIHash: cmd.String(flags.APIHash),
					}
					if cfg.APIID == 0 || cfg.APIHash == "" {
						return fmt.Errorf("%s and %s are required (set via env, flags, or 'config set')", flags.EnvTelegramAPIID, flags.EnvTelegramAPIHash)
					}
					return tgclient.Login(ctx, cfg, phone)
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
					cfg := &tgclient.Config{
						APIID:   cmd.Int(flags.APIID),
						APIHash: cmd.String(flags.APIHash),
					}
					if cfg.APIID == 0 || cfg.APIHash == "" {
						return fmt.Errorf("%s and %s are required (set via env, flags, or 'config set')", flags.EnvTelegramAPIID, flags.EnvTelegramAPIHash)
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
						Usage:     fmt.Sprintf("Store a config value securely (e.g., %s, %s)", flags.EnvTelegramAPIID, flags.EnvAnthropicAPIKey),
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
