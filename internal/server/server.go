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
	"strings"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/experimental-ext-variants/go/sdk/variants"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/completion"
	"github.com/tolmachov/mcp-telegram/internal/logging"
	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/prompts"
	"github.com/tolmachov/mcp-telegram/internal/resources"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
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

// stdioTransport builds the newline-delimited stdio MCP transport over the
// injected stdin/stdout (see the New doc comment for why they are injectable).
func (s *Server) stdioTransport() *mcp.IOTransport {
	return &mcp.IOTransport{
		Reader: io.NopCloser(s.stdin),
		Writer: nopWriteCloser{s.stdout},
	}
}

const happyInstructions = "Use SearchChats or GetChats to find chat IDs before calling other tools. Chat IDs are numeric. If you only have a username, use ResolveUsername to get the chat ID. When the user asks to message someone, always confirm the recipient before sending. When the user asks to summarize, digest, or recap a chat, call SummarizeChat rather than fetching messages with GetMessages and summarizing them yourself — it summarizes long histories server-side without loading every message into context."

// Surfaced as a JSON-RPC error on initialize so hosts render a connection
// error — the alternative (a silently-green server with no tools) is the
// failure mode this whole ceremony exists to avoid.
//
//nolint:gosec // G101: this is a user-facing help string naming the env vars, not a credential.
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
	Variant        string        // "" → expose all SEP-2053 variants; else pin one (full|compact|research)
	Transport      string        // "" or "stdio" → stdio; "http" → streamable HTTP on HTTPAddr
	HTTPAddr       string        // listen address for Transport == "http" (e.g. ":8080")
	LogFormat      string        // "json" | "text"; "" → json for http, text for stdio
	LogLevel       string        // "debug" | "info" | "warn" | "error"; "" → info
	// Auth enables the embedded OAuth authorization server with per-user
	// Telegram sessions (HTTP transport only). SessionStore is required with
	// it and holds those sessions.
	Auth         *authsrv.Config
	SessionStore sessionstore.Store
	Stdin        io.Reader
	Stdout       io.Writer
	ErrOut       io.Writer
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
	variant        string
	transport      string
	httpAddr       string
	authCfg        *authsrv.Config
	sessionStore   sessionstore.Store
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
	if !validVariant(opts.Variant) {
		return nil, fmt.Errorf("server.New: unknown variant %q; expected one of: %s (or empty for all)", opts.Variant, strings.Join(variantIDs(), ", "))
	}
	switch opts.Transport {
	case "", TransportStdio:
		opts.Transport = TransportStdio
	case TransportHTTP:
		if opts.HTTPAddr == "" {
			return nil, fmt.Errorf("server.New: Options.HTTPAddr is required for the http transport")
		}
	default:
		return nil, fmt.Errorf("server.New: unknown transport %q; expected %q or %q", opts.Transport, TransportStdio, TransportHTTP)
	}
	if opts.Auth != nil {
		if opts.Transport != TransportHTTP {
			return nil, fmt.Errorf("server.New: Options.Auth requires the http transport")
		}
		if opts.SessionStore == nil {
			return nil, fmt.Errorf("server.New: Options.SessionStore is required with Options.Auth")
		}
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
	level, err := logging.ParseLevel(opts.LogLevel)
	if err != nil {
		return nil, fmt.Errorf("server.New: %w", err)
	}
	logFormat := opts.LogFormat
	if logFormat == "" {
		// Default to structured JSON over HTTP (Cloud Logging), human text on
		// stdio/local.
		logFormat = logging.FormatText
		if opts.Transport == TransportHTTP {
			logFormat = logging.FormatJSON
		}
	}
	logger := slog.New(logging.NewHandler(opts.ErrOut, logFormat, level, "mcp-telegram", opts.Version)).
		With("component", "mcp-telegram")

	return &Server{
		logger:         logger,
		version:        opts.Version,
		tgConfig:       opts.Config,
		allowedPaths:   opts.AllowedPaths,
		summarizeCfg:   opts.SummarizeCfg,
		mediaMaxBytes:  opts.MediaMaxBytes,
		tgRateLimitRPS: opts.TGRateLimitRPS,
		pinnedRefresh:  opts.PinnedRefresh,
		variant:        opts.Variant,
		transport:      opts.Transport,
		httpAddr:       opts.HTTPAddr,
		authCfg:        opts.Auth,
		sessionStore:   opts.SessionStore,
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
		return s.startupError(ctx, missingCredentialsMessage)
	}

	// The auth mode has no ambient single-account client: each user's client
	// is connected lazily by the pool on their stored session.
	if s.authCfg != nil {
		return s.runHTTPWithAuth(ctx)
	}

	client, waiter, err := tgclient.CreateClient(s.tgConfig, s.floodWaitLogger())
	if err != nil {
		msg := fmt.Sprintf("mcp-telegram: failed to construct Telegram client: %v. Verify TELEGRAM_API_ID/HASH and the session file; `mcp-telegram logout` followed by `mcp-telegram login` often recovers a corrupt session.", err)
		s.logger.Error("refusing to start", "reason", "telegram client construction failed", "err", err)
		return s.startupError(ctx, msg)
	}

	err = waiter.Run(ctx, func(ctx context.Context) error {
		return client.Run(ctx, func(ctx context.Context) error {
			status, err := client.Auth().Status(ctx)
			if err != nil {
				msg := fmt.Sprintf("mcp-telegram: Telegram auth check failed: %v. Verify network connectivity and retry; if the error persists, `mcp-telegram logout` followed by `mcp-telegram login` may recover.", err)
				s.logger.Error("refusing to start", "reason", "auth check failed", "err", err)
				return s.startupError(ctx, msg)
			}

			if !status.Authorized {
				s.logger.Warn("refusing to start", "reason", "not authorized; login required")
				return s.startupError(ctx, notLoggedInMessage)
			}

			return s.runHappy(ctx, client)
		})
	})
	if err != nil {
		return fmt.Errorf("running server: %w", err)
	}
	return nil
}

// floodWaitLogger surfaces flood waits to the logs in a way that makes the
// absorbed-vs-surfaced decision visible: waits under the configured max are
// slept out and retried; longer ones fail fast rather than blocking past the
// MCP client's tool-call timeout. Write tools then render that failure as a
// retry-after error (via floodWaitResult); read tools surface the raw
// wrapped error.
func (s *Server) floodWaitLogger() tgclient.FloodWaitCallback {
	floodMaxWait := s.tgConfig.EffectiveFloodWaitMaxWait()
	return func(_ context.Context, d time.Duration) {
		if d > floodMaxWait {
			s.logger.Warn("telegram flood-wait exceeds max; failing fast",
				"wait_seconds", d.Seconds(),
				"max_wait_seconds", floodMaxWait.Seconds(),
				"reason", "Telegram rate limit longer than the configured auto-wait; the call fails fast (write tools render a retry-after error) instead of blocking past the client timeout",
			)
			return
		}
		s.logger.Warn("telegram flood-wait; waiting it out",
			"wait_seconds", d.Seconds(),
			"max_wait_seconds", floodMaxWait.Seconds(),
			"reason", "Telegram rate limit; the request will retry automatically after the wait",
		)
	}
}

// assembly is one complete set of MCP servers built around one Telegram
// client: either the SEP-2053 variants proxy (variants == nil ⇔ pinned) or a
// single pinned-variant server. The stdio path builds exactly one; the HTTP
// auth mode builds one per authenticated user.
type assembly struct {
	variants *variants.Server // non-nil when exposing all variants
	single   *mcp.Server      // non-nil when --variant pins one

	// pinnedServers are the inner servers the pinned-chat watcher mirrors
	// its resource set onto.
	pinnedServers []*mcp.Server
	msgProvider   *messages.Provider
}

// buildAssembly constructs handlers, resources, prompts, and the MCP
// server(s) for one Telegram client.
func (s *Server) buildAssembly(client *telegram.Client) (*assembly, error) {
	impl := &mcp.Implementation{Name: "mcp-telegram", Version: s.version}
	serverOpts := &mcp.ServerOptions{
		Instructions: happyInstructions,
		Logger:       s.logger,
		// Suggest chat titles/usernames/ids for prompt arguments and the
		// chat resource template as the user types.
		CompletionHandler: completion.Handler(client.API()),
	}

	// The RPS ceiling is configurable (0 → messages.DefaultRateLimitRPS) so
	// operators can loosen it when tools bottleneck on the shared limiter.
	// Raising it too high will trip Telegram's FLOOD_WAIT which the tgclient
	// waiter wrapper reports via onFloodWait.
	msgProvider := messages.NewProviderWithRate(client.API(), s.tgRateLimitRPS)

	// One chat-list snapshot shared by GetChats and SearchChats so a search
	// reuses an already-loaded listing instead of re-paginating every dialog.
	chatsCache := tools.NewChatsCache(client.API())

	fullHandlers, researchHandlers := s.buildHandlers(client.API(), msgProvider, chatsCache)

	// Resources, chat template, and prompts are read-only and identical across
	// variants, so register them on every inner server through one closure.
	wire := func(srv *mcp.Server) {
		resources.RegisterResources(srv, []resources.ResourceHandler{
			resources.NewMeHandler(client.API()),
			resources.NewChatsHandler(client.API()),
		})
		resources.RegisterChatTemplate(srv, client.API())
		prompts.Register(srv)
	}

	if s.variant == "" {
		vs, inners := buildVariantsServer(impl, serverOpts, fullHandlers, researchHandlers, wire, s.logger)
		return &assembly{variants: vs, pinnedServers: inners, msgProvider: msgProvider}, nil
	}
	d, ok := defForVariant(s.variant)
	if !ok {
		// Unreachable: New rejects unknown non-empty variants, and Server is
		// only constructible through New. Fail loudly rather than silently
		// falling back to the zero-value mode if that invariant is ever broken.
		return nil, fmt.Errorf("buildAssembly: variant %q not found in table (should have been rejected by New)", s.variant)
	}
	srv := newInnerForMode(impl, serverOpts, fullHandlers, researchHandlers, d.mode, wire, s.logger)
	return &assembly{single: srv, pinnedServers: []*mcp.Server{srv}, msgProvider: msgProvider}, nil
}

func (s *Server) runHappy(ctx context.Context, client *telegram.Client) error {
	asm, err := s.buildAssembly(client)
	if err != nil {
		return err
	}

	// run is the serve loop (stdio frames or streamable HTTP, per
	// --transport). With no --variant override we expose every variant via
	// the SEP-2053 proxy and mirror pinned resources onto all of them (one
	// poller). With an override we expose just that variant as a plain server.
	var run func() error
	if asm.variants != nil {
		if s.transport == TransportHTTP {
			run = func() error { return s.runVariantsHTTP(ctx, asm.variants, s.httpAddr) }
		} else {
			run = func() error { return asm.variants.Run(ctx, s.stdioTransport()) }
		}
		// The variants proxy cannot forward async resources/list_changed
		// notifications (they fire from the watcher goroutine on a background
		// context with no front session to redirect to — a documented library
		// limitation). Pinned resources are still exposed on every variant and
		// refreshed by the poller, so clients see the updated set on their next
		// resources/list; only proactive change-notifications are unavailable.
		// Pin a single --variant to restore live notifications.
		s.logger.Info("multi-variant mode: pinned-chat resources are exposed on every variant and refreshed by one poller, but live resources/list_changed notifications are not delivered through the variants proxy; pin a single --variant for live updates")
	} else {
		if s.transport == TransportHTTP {
			run = func() error { return s.runMCPHTTP(ctx, asm.single, s.httpAddr) }
		} else {
			run = func() error { return asm.single.Run(ctx, s.stdioTransport()) }
		}
	}
	pinnedServers := asm.pinnedServers
	msgProvider := asm.msgProvider

	// The SDK has no BeforeListResources hook, so the pinned-chat set is
	// refreshed by a periodic poller (default 30s, --pinned-refresh-seconds).
	// list_changed only fires when the set actually changes, so the ticker is
	// safe to run on a short interval. The watcher runs on watchCtx, a child of
	// ctx we cancel as soon as run() returns: the MCP host normally shuts us down
	// by closing stdin, which unblocks run() *without* canceling ctx, so without
	// this the watcher would never see Done() and every clean disconnect would
	// hit the 5s abandon timeout below.
	watchCtx, cancelWatch := context.WithCancel(ctx)
	pinnedProvider := resources.NewPinnedChatsProvider(client.API(), msgProvider, s.logger, pinnedServers...)
	pinnedDone := pinnedProvider.WatchInBackground(watchCtx, s.pinnedRefresh)

	runErr := run()
	cancelWatch()

	// Wait for the pinned-chat watcher to exit before returning, so its
	// goroutine cannot race with server teardown while mid-way through
	// AddResource/RemoveResources. cancelWatch above stops it deterministically;
	// the timeout only guards against a genuinely wedged provider (e.g. blocked
	// in a Telegram call) holding up shutdown indefinitely. If it ever fires we
	// are abandoning a live goroutine that will then touch a torn-down server —
	// a real correctness hazard, so it logs at Error, not Warn.
	if pinnedDone != nil {
		select {
		case <-pinnedDone:
		case <-time.After(5 * time.Second):
			s.logger.Error("pinned-chat watcher did not exit in 5s; abandoning", "pinned_refresh", s.pinnedRefresh)
		}
	}
	if runErr != nil {
		return fmt.Errorf("running MCP server: %w", runErr)
	}
	return nil
}

// buildHandlers constructs every tool handler once and returns the full set and
// the read-only research subset. The same handler instances are shared: they
// carry no per-server state, so registering one on several inner servers is
// safe. research holds tools that only read Telegram (search, fetch, summarize,
// export); the remaining 13 mutate state (send, edit, delete, forward, react,
// mark-as-read, join/leave, mute, and the four folder edits) and are excluded
// from the research variant.
func (s *Server) buildHandlers(api *tg.Client, msgProvider *messages.Provider, chatsCache *tools.ChatsCache) (full, research []tools.Handler) {
	research = []tools.Handler{
		tools.NewMeGetHandler(api),
		tools.NewChatsGetHandler(chatsCache),
		tools.NewChatsSearchHandler(api, chatsCache),
		tools.NewChatInfoGetHandler(api),
		tools.NewMessagesGetHandler(msgProvider),
		tools.NewMessagesSearchHandler(msgProvider),
		tools.NewMessagesSearchGlobalHandler(msgProvider),
		tools.NewMessageContextGetHandler(msgProvider),
		tools.NewGetRepliesHandler(msgProvider),
		tools.NewGetForumTopicsHandler(msgProvider),
		tools.NewUsernameResolveHandler(api),
		tools.NewMessageLinkResolveHandler(api),
		tools.NewMessageBackupHandler(api, msgProvider, s.allowedPaths),
		tools.NewChatSummarizeHandler(msgProvider, s.summarizeCfg),
		tools.NewMediaGetHandler(api, s.mediaMaxBytes),
		tools.NewGetFoldersHandler(api),
	}
	mutating := []tools.Handler{
		tools.NewMessageSendHandler(api),
		tools.NewMessageReadHandler(api),
		tools.NewMessageEditHandler(api),
		tools.NewMessageDeleteHandler(api),
		tools.NewMessageForwardHandler(api),
		tools.NewSetReactionHandler(api),
		tools.NewJoinChatHandler(api),
		tools.NewLeaveChatHandler(api),
		tools.NewChatMuteHandler(api),
		tools.NewCreateFolderHandler(api),
		tools.NewDeleteFolderHandler(api),
		tools.NewAddChatsToFolderHandler(api),
		tools.NewRemoveChatsFromFolderHandler(api),
	}
	full = make([]tools.Handler, 0, len(research)+len(mutating))
	full = append(full, research...)
	full = append(full, mutating...)
	return full, research
}

// startupError reports an unrecoverable startup failure in the way the active
// transport can deliver it. Over stdio the message is written as a JSON-RPC
// error on the initialize request — that is what MCP hosts like Claude
// Desktop render to the user. Over HTTP there is no MCP peer yet (nothing is
// listening), so fail fast with a plain error: the process exits non-zero and
// the operator/platform (e.g. Cloud Run) sees an unhealthy start instead of a
// listener that accepts connections it can never serve.
func (s *Server) startupError(ctx context.Context, message string) error {
	if s.transport == TransportHTTP {
		return errors.New(message)
	}
	return s.replyInitError(ctx, message)
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
