package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/prompts"
	"github.com/tolmachov/mcp-telegram/internal/resources"
	"github.com/tolmachov/mcp-telegram/internal/summarize"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tools"
)

// nopWriteCloser wraps an io.Writer in an io.WriteCloser with a no-op Close.
// Used to feed cli.Command.Writer (io.Writer) into mcp.IOTransport.Writer
// (io.WriteCloser). Closing the actual stdout would be incorrect in production
// and unnecessary for test pipes (the test closes its end of the pipe itself).
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// Options bundles the configuration for a Server so that New can grow new
// knobs (rate limits, polling intervals, …) without churning every call site.
// Required fields: Config, Version. Stdin/Stdout/ErrOut default to os.*
// when nil, mirroring the official SDK's StdioTransport behaviour.
type Options struct {
	Config         *tgclient.Config
	Version        string
	AllowedPaths   []string
	SummarizeCfg   summarize.Config
	MediaMaxBytes  int
	TGRateLimitRPS int           // 0 → use messages.DefaultRateLimitRPS
	PinnedRefresh  time.Duration // 0 → disable pinned-chat background watcher
	Stdin          io.Reader
	Stdout         io.Writer
	ErrOut         io.Writer
}

// Server represents the MCP server for Telegram.
type Server struct {
	mcpServer      *mcp.Server
	logger         *slog.Logger
	tgConfig       *tgclient.Config
	allowedPaths   []string
	summarizeCfg   summarize.Config
	mediaMaxBytes  int
	tgRateLimitRPS int
	pinnedRefresh  time.Duration
	stdin          io.Reader
	stdout         io.Writer
	errOut         io.Writer
}

// New creates a new MCP server from an Options bundle.
//
// stdin/stdout are kept on the Options struct for test injection: the
// official Go SDK's StdioTransport hardcodes os.Stdin / os.Stdout, but
// mcp.IOTransport accepts arbitrary Reader/Writer — which lets the
// integration test pipe in custom io.Pipe endpoints.
// ErrOut is used for slog-based diagnostics (server lifecycle, flood-wait).
func New(opts Options) (*Server, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("server.New: Options.Config is required")
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.ErrOut == nil {
		opts.ErrOut = os.Stderr
	}
	logger := slog.New(slog.NewTextHandler(opts.ErrOut, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})).With("component", "mcp-telegram")

	mcpServer := mcp.NewServer(
		&mcp.Implementation{
			Name:    "mcp-telegram",
			Version: opts.Version,
		},
		&mcp.ServerOptions{
			Instructions: "Use SearchChats or GetChats to find chat IDs before calling other tools. Chat IDs are numeric. If you only have a username, use ResolveUsername to get the chat ID. When the user asks to message someone, always confirm the recipient before sending.",
			Logger:       logger,
			// Capabilities are auto-inferred from AddTool / AddResource / AddPrompt
			// registrations — no need to declare them explicitly.
		},
	)

	return &Server{
		mcpServer:      mcpServer,
		logger:         logger,
		tgConfig:       opts.Config,
		allowedPaths:   opts.AllowedPaths,
		summarizeCfg:   opts.SummarizeCfg,
		mediaMaxBytes:  opts.MediaMaxBytes,
		tgRateLimitRPS: opts.TGRateLimitRPS,
		pinnedRefresh:  opts.PinnedRefresh,
		stdin:          opts.Stdin,
		stdout:         opts.Stdout,
		errOut:         opts.ErrOut,
	}, nil
}

// Run starts the MCP server over stdio.
func (s *Server) Run(ctx context.Context) error {
	// Surface flood-wait throttling to operators via slog. The previous SDK
	// could push these as MCP log notifications because the server held a
	// global handle; the official SDK only exposes log notifications on a
	// per-session basis, and this callback fires from a background goroutine
	// that has no active session in scope. Logging to slog is the spec-correct
	// fallback — operators see the warnings on stderr.
	onFloodWait := func(_ context.Context, d time.Duration) {
		s.logger.Warn("telegram flood-wait",
			"wait_seconds", d.Seconds(),
			"reason", "Telegram rate limit; the request will retry automatically",
		)
	}

	// Create a Telegram client with flood wait handling.
	client, waiter, err := tgclient.CreateClient(s.tgConfig, onFloodWait)
	if err != nil {
		return fmt.Errorf("creating Telegram client: %w", err)
	}

	// waiter.Run wraps client.Run to handle FLOOD_WAIT errors automatically.
	err = waiter.Run(ctx, func(ctx context.Context) error {
		return client.Run(ctx, func(ctx context.Context) error {
			// Check if authorized.
			status, err := client.Auth().Status(ctx)
			if err != nil {
				return fmt.Errorf("checking auth status: %w", err)
			}

			if !status.Authorized {
				return fmt.Errorf("not authorized, please run 'login' command first")
			}

			// Create a shared message provider with rate limiting. The RPS
			// ceiling is configurable (0 → messages.DefaultRateLimitRPS) so
			// operators can loosen it when tools bottleneck on the shared
			// limiter. Raising it too high will trip Telegram's FLOOD_WAIT
			// which the tgclient waiter wrapper reports via onFloodWait.
			msgProvider := messages.NewProviderWithRate(client.API(), s.tgRateLimitRPS)

			tools.RegisterTools(s.mcpServer, []tools.Handler{
				tools.NewMeGetHandler(client.API()),
				tools.NewChatsGetHandler(client.API()),
				tools.NewChatsSearchHandler(client.API()),
				tools.NewChatInfoGetHandler(client.API()),
				tools.NewMessagesGetHandler(msgProvider),
				tools.NewMessagesSearchHandler(msgProvider),
				tools.NewMessagesSearchGlobalHandler(msgProvider),
				tools.NewMessageContextGetHandler(msgProvider),
				tools.NewMessageSendHandler(client.API()),
				tools.NewMessageReadHandler(client.API()),
				tools.NewMessageEditHandler(client.API()),
				tools.NewMessageDeleteHandler(client.API()),
				tools.NewMessageForwardHandler(client.API()),
				tools.NewUsernameResolveHandler(client.API()),
				tools.NewMessageLinkResolveHandler(client.API()),
				tools.NewMessageBackupHandler(client.API(), msgProvider, s.allowedPaths),
				tools.NewChatMuteHandler(client.API()),
				tools.NewChatSummarizeHandler(msgProvider, s.summarizeCfg),
				tools.NewMediaGetHandler(client.API(), s.mediaMaxBytes),
			})

			resources.RegisterResources(s.mcpServer, []resources.ResourceHandler{
				resources.NewMeHandler(client.API()),
				resources.NewChatsHandler(client.API()),
			})
			resources.RegisterChatTemplate(s.mcpServer, client.API())

			prompts.Register(s.mcpServer)

			// Set up dynamic pinned chat resources via a background ticker.
			// The previous SDK supported a BeforeListResources hook that
			// refreshed the set on demand whenever a client called
			// resources/list; the official SDK has no equivalent hook, so
			// we tightened the polling interval from 60s to 30s to keep the
			// freshness window reasonable. The doRefresh signature diff
			// keeps the ticker idempotent — list_changed only fires when the
			// set actually changes.
			pinnedProvider := resources.NewPinnedChatsProvider(client.API(), msgProvider, s.mcpServer, s.logger)
			pinnedDone := pinnedProvider.WatchInBackground(ctx, s.pinnedRefresh)

			// Run the MCP server over the injected stdin/stdout. The official
			// SDK's StdioTransport hardcodes os.Stdin / os.Stdout, but
			// IOTransport accepts arbitrary Reader/Writer — which lets the
			// integration test pipe in custom io.Pipe endpoints while production
			// use (where stdin/stdout == os.Stdin / os.Stdout via urfave/cli)
			// behaves identically to StdioTransport over newline-delimited JSON.
			runErr := s.mcpServer.Run(ctx, &mcp.IOTransport{
				Reader: io.NopCloser(s.stdin),
				Writer: nopWriteCloser{s.stdout},
			})

			// Wait for the pinned-chat watcher to exit before returning, so
			// its goroutine cannot race with server teardown while mid-way
			// through AddResource/RemoveResources. WatchInBackground closes
			// pinnedDone as soon as the parent ctx is cancelled.
			if pinnedDone != nil {
				<-pinnedDone
			}
			return runErr
		})
	})
	if err != nil {
		return fmt.Errorf("running server: %w", err)
	}
	return nil
}
