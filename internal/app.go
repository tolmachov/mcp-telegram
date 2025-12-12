package internal

import (
	"context"
	"fmt"
	"io"

	"github.com/urfave/cli/v3"

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
					apiIDFlag(),
					apiHashFlag(),
					allowedPathsFlag(),
					summarizeProviderFlag(),
					summarizeModelFlag(),
					ollamaURLFlag(),
					geminiAPIKeyFlag(),
					anthropicAPIKeyFlag(),
					summarizeBatchTokensFlag(),
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					cfg := &tgclient.Config{
						APIID:   cmd.Int(flagAPIID),
						APIHash: cmd.String(flagAPIHash),
					}
					allowedPaths := cmd.StringSlice(flagAllowedPaths)
					summarizeCfg := summarize.Config{
						Provider:        summarize.ProviderName(cmd.String(flagSummarizeProvider)),
						Model:           cmd.String(flagSummarizeModel),
						OllamaURL:       cmd.String(flagOllamaURL),
						GeminiAPIKey:    cmd.String(flagGeminiAPIKey),
						AnthropicAPIKey: cmd.String(flagAnthropicAPIKey),
						BatchTokens:     cmd.Int(flagSummarizeBatchTokens),
					}
					srv, err := server.New(cfg, Version, allowedPaths, summarizeCfg, cmd.Root().Reader, cmd.Root().Writer, cmd.Root().ErrWriter)
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
					apiIDFlag(),
					apiHashFlag(),
					phoneFlag(),
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					phone := cmd.String(flagPhone)
					if phone == "" {
						return fmt.Errorf("phone number is required")
					}
					cfg := &tgclient.Config{
						APIID:   cmd.Int(flagAPIID),
						APIHash: cmd.String(flagAPIHash),
					}
					return tgclient.Login(ctx, cfg, phone)
				},
			},
			{
				Name:  "logout",
				Usage: "Logout from Telegram",
				Flags: []cli.Flag{
					apiIDFlag(),
					apiHashFlag(),
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					cfg := &tgclient.Config{
						APIID:   cmd.Int(flagAPIID),
						APIHash: cmd.String(flagAPIHash),
					}
					return tgclient.Logout(ctx, cfg)
				},
			},
		},
	}
}
