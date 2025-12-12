package server

import (
	"context"
	"fmt"
	"io"
	"log"

	"github.com/mark3labs/mcp-go/server"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/resources"
	"github.com/tolmachov/mcp-telegram/internal/summarize"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tools"
)

// Server represents the MCP server for Telegram
type Server struct {
	mcpServer    *server.MCPServer
	tgConfig     *tgclient.Config
	allowedPaths []string
	summarizeCfg summarize.Config
	stdin        io.Reader
	stdout       io.Writer
	errOut       io.Writer
}

// New creates a new MCP server
func New(cfg *tgclient.Config, version string, allowedPaths []string, summarizeCfg summarize.Config, stdin io.Reader, stdout, errOut io.Writer) (*Server, error) {
	mcpServer := server.NewMCPServer(
		"mcp-telegram",
		version,
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true),
	)

	// Enable sampling capability for LLM requests
	mcpServer.EnableSampling()

	return &Server{
		mcpServer:    mcpServer,
		tgConfig:     cfg,
		allowedPaths: allowedPaths,
		summarizeCfg: summarizeCfg,
		stdin:        stdin,
		stdout:       stdout,
		errOut:       errOut,
	}, nil
}

// Run starts the MCP server over stdio
func (s *Server) Run(ctx context.Context) error {
	// Create a Telegram client with flood wait handling
	client, waiter := tgclient.CreateClient(s.tgConfig)

	// waiter.Run wraps a client.Run to handle FLOOD_WAIT errors automatically
	err := waiter.Run(ctx, func(ctx context.Context) error {
		return client.Run(ctx, func(ctx context.Context) error {
			// Check if authorized
			status, err := client.Auth().Status(ctx)
			if err != nil {
				return fmt.Errorf("checking auth status: %w", err)
			}

			if !status.Authorized {
				return fmt.Errorf("not authorized, please run 'login' command first")
			}

			// Create shared message provider with rate limiting
			msgProvider := messages.NewProvider(client.API())

			tools.RegisterTools(s.mcpServer, []tools.Handler{
				tools.NewMeGetHandler(client.API()),
				tools.NewChatsGetHandler(client.API()),
				tools.NewChatsSearchHandler(client.API()),
				tools.NewChatInfoGetHandler(client.API()),
				tools.NewMessagesGetHandler(msgProvider),
				tools.NewMessageDraftHandler(client.API()),
				tools.NewMessageSendHandler(client.API()),
				tools.NewMessageScheduleHandler(client.API()),
				tools.NewScheduledGetHandler(client.API()),
				tools.NewScheduledDeleteHandler(client.API()),
				tools.NewUsernameResolveHandler(client.API()),
				tools.NewMessageBackupHandler(client.API(), msgProvider, s.allowedPaths),
				tools.NewChatMuteHandler(client.API()),
				tools.NewChatUnmuteHandler(client.API()),
				tools.NewChatSummarizeHandler(msgProvider, s.mcpServer, s.summarizeCfg),
			})

			resources.RegisterResources(s.mcpServer,
				[]resources.ResourceHandler{
					resources.NewMeHandler(client.API()),
					resources.NewChatsHandler(client.API()),
				},
				[]resources.ResourceTemplateHandler{
					resources.NewChatMessagesHandler(msgProvider),
					resources.NewChatInfoHandler(client.API()),
				},
			)

			// Run MCP server over stdio
			errLogger := log.New(s.errOut, "[mcp-telegram] ", log.LstdFlags)
			stdioServer := server.NewStdioServer(s.mcpServer)
			stdioServer.SetErrorLogger(errLogger)

			return stdioServer.Listen(ctx, s.stdin, s.stdout)
		})
	})
	if err != nil {
		return fmt.Errorf("running server: %w", err)
	}
	return nil
}
