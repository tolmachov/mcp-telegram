package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/completion"
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

const happyInstructions = "Use SearchChats or GetChats to find chat IDs before calling other tools. Chat IDs are numeric. If you only have a username, use ResolveUsername to get the chat ID. When the user asks to message someone, always confirm the recipient before sending."

// Surfaced as a JSON-RPC error on initialize so hosts render a connection
// error — the alternative (a silently-green server with no tools) is the
// failure mode this whole ceremony exists to avoid.
const missingCredentialsMessage = "mcp-telegram is not configured: TELEGRAM_API_ID and TELEGRAM_API_HASH are required. Set them via environment variables, a .env file, CLI flags (--api-id / --api-hash), or `mcp-telegram config set api-id <id>` / `mcp-telegram config set api-hash <hash>`. You can obtain an API ID/Hash from https://my.telegram.org."

const notLoggedInMessage = "mcp-telegram is not logged in to Telegram. Run `mcp-telegram login --phone <+countrycode…>` in a terminal to authenticate, then restart this MCP server."

// -32002 is in the JSON-RPC server-defined range (-32000 to -32099 per
// §5.1), so hosts can distinguish "server not ready" from the standard
// parse/invalid-request/internal-error codes.
const initErrorCode = -32002

// Options configures New. Only Config is required; Version is recommended
// (propagated to mcp.Implementation.Version). Stdin/Stdout/ErrOut default
// to os.* when nil, mirroring the official SDK's StdioTransport.
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

type Server struct {
	logger         *slog.Logger
	version        string
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

	return &Server{
		logger:         logger,
		version:        opts.Version,
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

// Run starts the MCP server over stdio. When Telegram is unavailable
// (missing credentials, client construction failure, or not-logged-in), Run
// returns a non-nil error after writing a JSON-RPC error on the initialize
// request — the caller (main) turns that into a non-zero exit. This is what
// hosts like Claude Desktop actually render to the user; silently
// registering a stub tool would leave the server visible-but-useless in the
// UI, which is the failure mode we're trying to avoid.
func (s *Server) Run(ctx context.Context) error {
	if s.tgConfig.APIID == 0 || s.tgConfig.APIHash == "" {
		s.logger.Warn("refusing to start", "reason", "missing Telegram API credentials")
		return s.replyInitError(ctx, missingCredentialsMessage)
	}

	onFloodWait := func(_ context.Context, d time.Duration) {
		s.logger.Warn("telegram flood-wait",
			"wait_seconds", d.Seconds(),
			"reason", "Telegram rate limit; the request will retry automatically",
		)
	}

	client, waiter, err := tgclient.CreateClient(s.tgConfig, onFloodWait)
	if err != nil {
		msg := fmt.Sprintf("mcp-telegram: failed to construct Telegram client: %v. Verify TELEGRAM_API_ID/HASH and the session file; `mcp-telegram logout` followed by `mcp-telegram login` often recovers a corrupt session.", err)
		s.logger.Error("refusing to start", "reason", "telegram client construction failed", "err", err)
		return s.replyInitError(ctx, msg)
	}

	err = waiter.Run(ctx, func(ctx context.Context) error {
		return client.Run(ctx, func(ctx context.Context) error {
			status, err := client.Auth().Status(ctx)
			if err != nil {
				msg := fmt.Sprintf("mcp-telegram: Telegram auth check failed: %v. Verify network connectivity and retry; if the error persists, `mcp-telegram logout` followed by `mcp-telegram login` may recover.", err)
				s.logger.Error("refusing to start", "reason", "auth check failed", "err", err)
				return s.replyInitError(ctx, msg)
			}

			if !status.Authorized {
				s.logger.Warn("refusing to start", "reason", "not authorized; login required")
				return s.replyInitError(ctx, notLoggedInMessage)
			}

			return s.runHappy(ctx, client)
		})
	})
	if err != nil {
		return fmt.Errorf("running server: %w", err)
	}
	return nil
}

func (s *Server) runHappy(ctx context.Context, client *telegram.Client) error {
	mcpServer := mcp.NewServer(
		&mcp.Implementation{Name: "mcp-telegram", Version: s.version},
		&mcp.ServerOptions{
			Instructions: happyInstructions,
			Logger:       s.logger,
			// Suggest chat titles/usernames/ids for prompt arguments and the
			// chat resource template as the user types.
			CompletionHandler: completion.Handler(client.API()),
		},
	)

	// The RPS ceiling is configurable (0 → messages.DefaultRateLimitRPS) so
	// operators can loosen it when tools bottleneck on the shared limiter.
	// Raising it too high will trip Telegram's FLOOD_WAIT which the tgclient
	// waiter wrapper reports via onFloodWait.
	msgProvider := messages.NewProviderWithRate(client.API(), s.tgRateLimitRPS)

	tools.RegisterTools(mcpServer, []tools.Handler{
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
		tools.NewSetReactionHandler(client.API()),
		tools.NewUsernameResolveHandler(client.API()),
		tools.NewMessageLinkResolveHandler(client.API()),
		tools.NewMessageBackupHandler(client.API(), msgProvider, s.allowedPaths),
		tools.NewChatMuteHandler(client.API()),
		tools.NewChatSummarizeHandler(msgProvider, s.summarizeCfg),
		tools.NewMediaGetHandler(client.API(), s.mediaMaxBytes),
	})

	resources.RegisterResources(mcpServer, []resources.ResourceHandler{
		resources.NewMeHandler(client.API()),
		resources.NewChatsHandler(client.API()),
	})
	resources.RegisterChatTemplate(mcpServer, client.API())

	prompts.Register(mcpServer)

	// The SDK has no BeforeListResources hook, so the pinned-chat set is
	// refreshed by a 30s poller. list_changed only fires when the set
	// actually changes, so the ticker is safe to run on a short interval.
	pinnedProvider := resources.NewPinnedChatsProvider(client.API(), msgProvider, mcpServer, s.logger)
	pinnedDone := pinnedProvider.WatchInBackground(ctx, s.pinnedRefresh)

	runErr := mcpServer.Run(ctx, &mcp.IOTransport{
		Reader: io.NopCloser(s.stdin),
		Writer: nopWriteCloser{s.stdout},
	})

	// Wait for the pinned-chat watcher to exit before returning, so its
	// goroutine cannot race with server teardown while mid-way through
	// AddResource/RemoveResources. WatchInBackground closes pinnedDone on
	// parent-ctx cancellation; the timeout guards against a wedged provider
	// holding up shutdown indefinitely.
	if pinnedDone != nil {
		select {
		case <-pinnedDone:
		case <-time.After(5 * time.Second):
			s.logger.Warn("pinned-chat watcher did not exit in 5s; abandoning", "pinned_refresh", s.pinnedRefresh)
		}
	}
	return runErr
}

// replyInitError reads newline-delimited JSON-RPC frames from stdin until it
// sees an `initialize` request, then writes a JSON-RPC error response echoing
// the request's id and returns. Per MCP lifecycle, `initialize` is the only
// valid frame at this stage — `notifications/initialized` comes after the
// initialize response — but we drop any other frames leniently.
//
// This bypasses the SDK on purpose: the official Go SDK's Server auto-handles
// initialize and has no hook for replacing the response with an error, but
// returning a JSON-RPC error on initialize is exactly what the spec's
// Lifecycle document recommends for unrecoverable startup failures. Pulling
// off a single frame and responding by hand is a small amount of code and
// keeps the SDK surface clean for the happy path.
//
// When stdin is a TTY (the operator is running the binary directly rather
// than an MCP host spawning it), we short-circuit: there's no peer to receive
// the JSON-RPC error, and waiting for an initialize frame that will never
// arrive would hang the process through Ctrl-C. Returning the message as a
// plain Go error lets urfave/cli print it to stderr and exit cleanly.
func (s *Server) replyInitError(ctx context.Context, message string) error {
	if isTTY(s.stdin) {
		return errors.New(message)
	}

	type frame struct {
		line []byte
		err  error
	}
	// The reader goroutine may outlive this function: if ctx is cancelled
	// while it's parked inside bufio.Reader.ReadBytes (no ctx deadline), it
	// stays alive until stdin closes. That's a bounded leak in production
	// (main exits and reaps it) and unblocks in tests when the pipe writer
	// closes. Don't close s.stdin here — it may be os.Stdin shared with the
	// rest of the process.
	ch := make(chan frame)
	go func() {
		reader := bufio.NewReader(s.stdin)
		for {
			line, err := reader.ReadBytes('\n')
			if len(line) > 0 {
				select {
				case ch <- frame{line: line}:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				select {
				case ch <- frame{err: err}:
				case <-ctx.Done():
				}
				return
			}
		}
	}()

	// Warn-level logs (below) surface protocol violations at the default Info
	// threshold, so an operator debugging "why did my host time out" sees them.
	for {
		select {
		case <-ctx.Done():
			// The host disconnected (or was killed) before sending initialize,
			// so the pending error message never made it out. Log it so an
			// operator sees *why* startup aborted, not just the bare ctx.Err.
			s.logger.Warn("startup aborted before initialize received", "pending_error", message, "err", ctx.Err())
			return ctx.Err()
		case f := <-ch:
			if f.err != nil {
				if errors.Is(f.err, io.EOF) {
					return io.ErrUnexpectedEOF
				}
				return fmt.Errorf("reading initialize request: %w", f.err)
			}
			var req struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Method  string          `json:"method"`
			}
			if err := json.Unmarshal(f.line, &req); err != nil {
				s.logger.Warn("dropping unparseable pre-initialize frame", "err", err)
				continue
			}
			if req.Method == "initialize" && len(req.ID) > 0 {
				return s.writeInitErrorResponse(req.ID, message)
			}
			s.logger.Warn("dropping non-initialize pre-initialize frame", "method", req.Method)
		}
	}
}

func isTTY(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	stat, err := f.Stat()
	if err != nil {
		return false
	}
	return stat.Mode()&os.ModeCharDevice != 0
}

// writeInitErrorResponse emits a single JSON-RPC error frame and returns a
// non-nil error so the process exits non-zero, which is what hosts watch
// for to decide "server failed to start".
func (s *Server) writeInitErrorResponse(id json.RawMessage, message string) error {
	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{
		JSONRPC: "2.0",
		ID:      id,
	}
	resp.Error.Code = initErrorCode
	resp.Error.Message = message

	encoded, err := json.Marshal(resp)
	if err != nil {
		s.logger.Error("marshalling init error", "err", err)
		return fmt.Errorf("marshalling init error: %w", err)
	}
	encoded = append(encoded, '\n')

	// Loop until fully written: io.Writer contract permits short writes with
	// err==nil, and a truncated frame would reach the host as the exact
	// "Server disconnected" this function exists to avoid. Guard against
	// pathological `n==0 && err==nil` writers so we fail loudly instead of
	// spinning forever.
	for written := 0; written < len(encoded); {
		n, werr := s.stdout.Write(encoded[written:])
		if werr != nil {
			s.logger.Error("writing init error frame", "err", werr, "bytes_written", written+n)
			return fmt.Errorf("writing init error: %w", werr)
		}
		if n == 0 {
			s.logger.Error("init error frame writer made no progress", "bytes_written", written)
			return fmt.Errorf("writing init error: writer made no progress")
		}
		written += n
	}
	return fmt.Errorf("mcp-telegram refused to start: %s", message)
}
