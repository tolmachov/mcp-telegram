package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/tolmachov/mcp-telegram/internal/tgdata"
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
		Reader: io.NopCloser(s.opts.Stdin),
		Writer: nopWriteCloser{s.opts.Stdout},
	}
}

const happyInstructions = "Use SearchChats or GetChats to find chat IDs before calling other tools. Chat IDs are numeric. If you only have a username, use ResolveUsername to get the chat ID. When the user asks to message someone, always confirm the recipient before sending. When the user asks to summarize, digest, or recap a chat, call SummarizeChat rather than fetching messages with GetMessages and summarizing them yourself — it summarizes long histories server-side without loading every message into context."

// The blocked-startup messages. Over stdio they become the login-required
// server's instructions and tool description (so the model reads them) and
// the tool's own re-check detail; over HTTP and on a TTY they are printed as
// the process's exit error.
//
//nolint:gosec // G101: this is a user-facing help string naming the env vars, not a credential.
const missingCredentialsMessage = "mcp-telegram is not configured: MCP_TELEGRAM_API_ID and MCP_TELEGRAM_API_HASH are required. Set them via process environment, CLI flags (--api-id / --api-hash), or `mcp-telegram config set api-id <id>` / `mcp-telegram config set api-hash <hash>`. You can obtain an API ID/Hash from https://my.telegram.org."

const notLoggedInMessage = "mcp-telegram is not logged in to Telegram — the stored session is missing, expired, or was revoked from Telegram's Devices/Active sessions list. Run `mcp-telegram login --phone <+countrycode…>` in a terminal to authenticate, then " + reconnectHint

// Options configures New. Every field is passed through from the resolved
// CLI flags; New only validates the combination.
type Options struct {
	Config         *tgclient.Config
	Version        string
	AllowedPaths   []string // --allowed-paths; empty → the OS backup directory (see backupAllowedPaths)
	SummarizeCfg   summarize.Config
	MediaMaxBytes  int
	TGRateLimitRPS int
	PinnedRefresh  time.Duration // 0 → disable pinned-chat background watcher
	Variant        string        // "" → expose all SEP-2053 variants; else pin one (full|compact|research)
	Transport      string        // "stdio", or "http" → streamable HTTP on HTTPAddr
	HTTPAddr       string        // listen address for Transport == "http" (e.g. ":8080")
	LogFormat      string        // "json" | "text"; "" → json for http, text for stdio
	LogLevel       string        // "debug" | "info" | "warn" | "error"
	// Auth enables the embedded OAuth authorization server with per-user
	// Telegram sessions. It and SessionStore (which holds those sessions) are
	// required with the http transport and rejected with stdio.
	Auth         *authsrv.Config
	SessionStore sessionstore.Store
	Stdin        io.Reader
	Stdout       io.Writer
	ErrOut       io.Writer
}

type Server struct {
	logger *slog.Logger
	opts   Options

	// authProbeFn is the live authorization re-check the login-required tool
	// performs, injectable so the tool's states can be exercised without a
	// Telegram connection (and, on darwin, without a Keychain prompt). New
	// points it at Server.authProbe; only tests replace it.
	authProbeFn func(context.Context) (account string, authorized bool, err error)
	// probeMu serializes authProbe — see its doc comment for why concurrent
	// probes on one stored session are a hazard rather than just waste.
	probeMu sync.Mutex
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
	case TransportStdio:
	case TransportHTTP:
		if opts.HTTPAddr == "" {
			return nil, fmt.Errorf("server.New: Options.HTTPAddr is required for the http transport")
		}
		if opts.Auth == nil || opts.SessionStore == nil {
			return nil, fmt.Errorf("server.New: the http transport requires Options.Auth and Options.SessionStore")
		}
	default:
		return nil, fmt.Errorf("server.New: unknown transport %q; expected %q or %q", opts.Transport, TransportStdio, TransportHTTP)
	}
	if opts.Auth != nil && opts.Transport != TransportHTTP {
		return nil, fmt.Errorf("server.New: Options.Auth requires the http transport")
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

	srv := &Server{logger: logger, opts: opts}
	srv.authProbeFn = srv.authProbe
	return srv, nil
}

// Run starts the MCP server on the configured transport (stdio, or streamable
// HTTP behind the embedded OAuth server). When Telegram cannot be reached at all — missing credentials, client
// construction failure, a connect-phase failure, a failed auth check, or a
// session that is simply not authorized — the stdio path comes up in
// login-required mode instead of failing: a server exposing one loudly-named
// tool that reports the problem, plus instructions that say the same thing to
// the model. See runLoginRequired for why that beats failing the connection.
//
// Over HTTP only the missing-credentials condition can arise, because the
// per-user clients are connected lazily by the pool; with no MCP peer to tell,
// it fails the process.
func (s *Server) Run(ctx context.Context) error {
	if s.opts.Config.APIID == 0 || s.opts.Config.APIHash == "" {
		s.logger.Warn("no Telegram access", "reason", "missing Telegram API credentials")
		return s.startBlocked(ctx, missingCredentialsMessage)
	}

	// HTTP has no ambient single-account client: each user's client is
	// connected lazily by the pool on their stored session.
	if s.opts.Transport == TransportHTTP {
		return s.runHTTPWithAuth(ctx)
	}

	client, waiter, err := tgclient.CreateClient(s.opts.Config, s.floodWaitLogger())
	if err != nil {
		msg := fmt.Sprintf("mcp-telegram: failed to construct Telegram client: %v. Verify MCP_TELEGRAM_API_ID/MCP_TELEGRAM_API_HASH and the session file; `mcp-telegram logout` followed by `mcp-telegram login` often recovers a corrupt session.", err)
		s.logger.Error("no Telegram access", "reason", "telegram client construction failed", "err", err)
		return s.startBlocked(ctx, msg)
	}

	// A blocked condition detected by our own callback is reported by
	// returning a *blockedError rather than serving from inside client.Run:
	// that unwinds the Telegram connection first, so the login-required server
	// does not hold a pointless connection open for the life of the MCP
	// session.
	//
	// served flips immediately before runHappy takes over, which is what makes
	// finishRun's phase test meaningful — see its doc comment. Both joins
	// (floodwait's waiter and gotd's errgroup) already establish the
	// happens-before, so a plain bool would be correct; atomic.Bool is
	// deliberate belt-and-braces on the one flag that decides whether a hard
	// failure gets downgraded to a degraded-but-running server.
	var served atomic.Bool
	err = waiter.Run(ctx, func(ctx context.Context) error {
		return client.Run(ctx, func(ctx context.Context) error {
			status, err := client.Auth().Status(ctx)
			if err != nil {
				msg := fmt.Sprintf("mcp-telegram: Telegram auth check failed: %v. Verify network connectivity and retry; if the error persists, `mcp-telegram logout` followed by `mcp-telegram login` may recover.", err)
				s.logger.Error("no Telegram access", "reason", "auth check failed", "err", err)
				return &blockedError{message: msg}
			}

			if !status.Authorized {
				s.logger.Warn("no Telegram access", "reason", "not authorized; login required")
				return &blockedError{message: notLoggedInMessage}
			}

			served.Store(true)
			return s.runHappy(ctx, client)
		})
	})
	return s.finishRun(ctx, err, served.Load())
}

// finishRun decides whether an error out of the Telegram client run is a
// blocked *startup* — which stdio answers with login-required mode — or a
// genuine serve-loop failure, which is always fatal.
//
// The test is the phase, not the error type. Classifying by *blockedError
// alone would cover only the two conditions our own callback can detect, and
// gotd never runs that callback for a whole class of failures: the callback
// fires on <-c.ready.Ready(), while restoreConnection ("corrupted key") returns
// before the errgroup starts, and reconnectUntilClosed turns
// AUTH_KEY_UNREGISTERED / SESSION_EXPIRED / AUTH_KEY_DUPLICATED / any 401 into
// backoff.Permanent (telegram/connect.go:94-106) in a sibling goroutine. Those
// are exactly the revoked-session cases notLoggedInMessage promises to handle,
// so routing them to a hard exit would leave the host showing the bare
// connection failure this whole mode exists to eliminate.
//
// (A session revoked from Telegram's Devices list often surfaces the friendlier
// way instead — auth.Status maps a 401 on users.getUsers to Status{} with no
// error, so the callback does run and produces a *blockedError. Both routes
// have to land in the same place.)
func (s *Server) finishRun(ctx context.Context, err error, served bool) error {
	if err == nil {
		return nil
	}
	if served {
		return fmt.Errorf("running server: %w", err)
	}
	var blocked *blockedError
	if errors.As(err, &blocked) {
		return s.startBlocked(ctx, blocked.message)
	}
	msg := fmt.Sprintf("mcp-telegram: could not connect to Telegram: %v. If this is a network problem, verify connectivity and retry; if the stored session was revoked or is corrupt, `mcp-telegram logout` followed by `mcp-telegram login` recovers it.", err)
	s.logger.Error("no Telegram access", "reason", "connect phase failed before the auth check ran", "err", err)
	return s.startBlocked(ctx, msg)
}

// floodWaitLogger surfaces flood waits to the logs in a way that makes the
// absorbed-vs-surfaced decision visible: waits under the configured max are
// slept out and retried; longer ones fail fast rather than blocking past the
// MCP client's tool-call timeout. Every tool then renders that failure as a
// retry-after error (via tools.floodWaitMessage).
func (s *Server) floodWaitLogger() tgclient.FloodWaitCallback {
	floodMaxWait := s.opts.Config.FloodWaitMaxWait
	return func(_ context.Context, d time.Duration) {
		if d > floodMaxWait {
			s.logger.Warn("telegram flood-wait exceeds max; failing fast",
				"wait_seconds", d.Seconds(),
				"max_wait_seconds", floodMaxWait.Seconds(),
				"reason", "Telegram rate limit longer than the configured auto-wait; the call fails fast (the tool renders a retry-after error) instead of blocking past the client timeout",
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
	// One chat-list snapshot shared by GetChats, SearchChats, the chats
	// resource and completion, so none of them re-paginates every dialog on
	// its own.
	api := client.API()
	chatsCache := tgdata.NewChatsCache(func(ctx context.Context, onProgress tgdata.ProgressFunc) (*tgdata.ChatsList, error) {
		return tgdata.GetChats(ctx, api, onProgress)
	})

	impl := &mcp.Implementation{Name: "mcp-telegram", Version: s.opts.Version}
	serverOpts := &mcp.ServerOptions{
		Instructions: happyInstructions,
		Logger:       s.logger,
		// Suggest chat titles/usernames/ids for prompt arguments and the
		// chat resource template as the user types.
		CompletionHandler: completion.Handler(chatsCache),
	}

	// One peer resolver — cache, stale-hash retry and rate limiter — shared by
	// every tool, resource and the message provider. The RPS ceiling is
	// configurable (--tg-rate-limit-rps) so operators can loosen it when tools
	// bottleneck on the shared limiter. Raising it too high will trip
	// Telegram's FLOOD_WAIT which the tgclient waiter wrapper reports via
	// onFloodWait.
	peers := tgclient.NewResolver(api, s.opts.TGRateLimitRPS)
	msgProvider := messages.NewProvider(peers)

	fullHandlers, researchHandlers := s.buildHandlers(api, peers, msgProvider, chatsCache)

	// Resources, chat template, and prompts are read-only and identical across
	// variants, so register them on every inner server through one closure.
	wire := func(srv *mcp.Server) {
		resources.RegisterResources(srv, []resources.ResourceHandler{
			resources.NewMeHandler(api),
			resources.NewChatsHandler(chatsCache),
		})
		resources.RegisterChatTemplate(srv, peers)
		prompts.Register(srv)
	}

	if s.opts.Variant == "" {
		vs, inners := buildVariantsServer(impl, serverOpts, fullHandlers, researchHandlers, wire, s.logger)
		return &assembly{variants: vs, pinnedServers: inners, msgProvider: msgProvider}, nil
	}
	d, ok := defForVariant(s.opts.Variant)
	if !ok {
		// Unreachable: New rejects unknown non-empty variants, and Server is
		// only constructible through New. Fail loudly rather than silently
		// falling back to the zero-value mode if that invariant is ever broken.
		return nil, fmt.Errorf("buildAssembly: variant %q not found in table (should have been rejected by New)", s.opts.Variant)
	}
	srv := newInnerForMode(impl, serverOpts, fullHandlers, researchHandlers, d.mode, wire, s.logger)
	return &assembly{single: srv, pinnedServers: []*mcp.Server{srv}, msgProvider: msgProvider}, nil
}

func (s *Server) runHappy(ctx context.Context, client *telegram.Client) error {
	asm, err := s.buildAssembly(client)
	if err != nil {
		return err
	}

	// run is the stdio serve loop; HTTP never reaches here (it serves per-user
	// assemblies through runHTTPWithAuth). With no --variant override we expose
	// every variant via the SEP-2053 proxy and mirror pinned resources onto all
	// of them (one poller). With an override we expose just that variant as a
	// plain server.
	var run func() error
	if asm.variants != nil {
		run = func() error { return asm.variants.Run(ctx, s.stdioTransport()) }
		// The variants proxy cannot forward async resources/list_changed
		// notifications (they fire from the watcher goroutine on a background
		// context with no front session to redirect to — a documented library
		// limitation). Pinned resources are still exposed on every variant and
		// refreshed by the poller, so clients see the updated set on their next
		// resources/list; only proactive change-notifications are unavailable.
		// Pin a single --variant to restore live notifications.
		s.logger.Info("multi-variant mode: pinned-chat resources are exposed on every variant and refreshed by one poller, but live resources/list_changed notifications are not delivered through the variants proxy; pin a single --variant for live updates")
	} else {
		run = func() error { return asm.single.Run(ctx, s.stdioTransport()) }
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
	pinnedDone := pinnedProvider.WatchInBackground(watchCtx, s.opts.PinnedRefresh)

	runErr := run()
	cancelWatch()

	// Wait for the pinned-chat watcher to exit before returning, so its
	// goroutine cannot race with server teardown while mid-way through
	// AddResource/RemoveResources. cancelWatch above stops it deterministically;
	// the timeout only guards against a genuinely wedged provider (e.g. blocked
	// in a Telegram call) holding up shutdown indefinitely. If it ever fires we
	// are abandoning a live goroutine that will then touch a torn-down server —
	// a real correctness hazard, so it logs at Error, not Warn.
	select {
	case <-pinnedDone:
	case <-time.After(5 * time.Second):
		s.logger.Error("pinned-chat watcher did not exit in 5s; abandoning", "pinned_refresh", s.opts.PinnedRefresh)
	}
	if runErr != nil {
		return fmt.Errorf("running MCP server: %w", runErr)
	}
	return nil
}

// buildHandlers constructs every tool handler once and returns the full set and
// the read-only research subset. The same handler instances are shared: they
// carry no per-server state, so registering one on several inner servers is
// safe. research holds tools that do not mutate Telegram or the local
// filesystem; the remaining tools mutate Telegram state. BackupMessages is
// local-stdio-only and is exposed solely by the full variant.
// The remaining 13 mutate state (send, edit, delete, forward, react,
// mark-as-read, join/leave, mute, and the four folder edits) and are excluded
// from the research variant.
func (s *Server) buildHandlers(api *tg.Client, peers *tgclient.Resolver, msgProvider *messages.Provider, chatsCache *tgdata.ChatsCache) (full, research []tools.Handler) {
	research = []tools.Handler{
		tools.NewMeGetHandler(api),
		tools.NewChatsGetHandler(chatsCache),
		tools.NewChatsSearchHandler(api, chatsCache),
		tools.NewChatInfoGetHandler(peers),
		tools.NewMessagesGetHandler(msgProvider),
		tools.NewMessagesSearchHandler(msgProvider),
		tools.NewMessagesSearchGlobalHandler(msgProvider),
		tools.NewMessageContextGetHandler(msgProvider),
		tools.NewGetRepliesHandler(msgProvider),
		tools.NewGetForumTopicsHandler(msgProvider),
		tools.NewUsernameResolveHandler(api),
		tools.NewMessageLinkResolveHandler(api),
		tools.NewChatSummarizeHandler(msgProvider, s.opts.SummarizeCfg),
		tools.NewMediaGetHandler(api, s.opts.MediaMaxBytes),
		tools.NewGetFoldersHandler(api),
	}
	mutating := []tools.Handler{
		tools.NewMessageSendHandler(peers),
		tools.NewMessageReadHandler(peers),
		tools.NewMessageEditHandler(peers),
		tools.NewMessageDeleteHandler(peers),
		tools.NewMessageForwardHandler(peers),
		tools.NewSetReactionHandler(peers),
		tools.NewJoinChatHandler(peers),
		tools.NewLeaveChatHandler(peers),
		tools.NewChatMuteHandler(peers),
		tools.NewCreateFolderHandler(peers),
		tools.NewDeleteFolderHandler(api),
		tools.NewAddChatsToFolderHandler(peers),
		tools.NewRemoveChatsFromFolderHandler(peers),
	}
	full = make([]tools.Handler, 0, len(research)+len(mutating)+1)
	full = append(full, research...)
	// BackupMessages writes to the local filesystem, so it is offered only on
	// stdio, and not when the research variant is pinned: that variant never
	// serves the full set, so it must not resolve (and create) the default
	// backup directory either.
	if s.opts.Transport != TransportHTTP && s.opts.Variant != variantResearch {
		full = append(full, tools.NewMessageBackupHandler(peers, msgProvider, s.backupAllowedPaths()))
	}
	full = append(full, mutating...)
	return full, research
}

// backupAllowedPaths returns --allowed-paths, or the OS backup directory when
// it is unset. The default is resolved here rather than at flag construction
// so help/version never touch the environment. If it cannot be determined the
// list stays empty, and BackupMessages then reports "no allowed paths
// configured" with guidance.
func (s *Server) backupAllowedPaths() []string {
	if len(s.opts.AllowedPaths) > 0 {
		return s.opts.AllowedPaths
	}
	d, err := tools.DefaultBackupDir()
	if err != nil {
		s.logger.Warn("could not determine default backup directory; BackupMessages will require --allowed-paths", "err", err)
		return nil
	}
	return []string{d}
}

// startBlocked reports a condition that leaves the process without Telegram
// access, in the way the active transport can actually deliver it.
//
// Over stdio the server comes up in login-required mode (see
// runLoginRequired): the host shows a connected server whose single tool and
// instructions name the problem. Over HTTP there is no MCP peer to tell —
// nothing is listening yet — so fail fast with a plain error: the process
// exits non-zero and the operator/platform (e.g. Cloud Run) sees an unhealthy
// start instead of a listener that accepts connections it can never serve.
//
// Running the binary by hand in a terminal short-circuits too. There is no
// MCP client on the other end of a TTY, so serving frames nobody will send
// would just hang; returning the message lets urfave/cli print it to stderr
// and exit — which is also what makes `mcp-telegram run` usable as a manual
// smoke test.
func (s *Server) startBlocked(ctx context.Context, message string) error {
	if s.opts.Transport == TransportHTTP || isTTY(s.opts.Stdin) {
		return errors.New(message)
	}
	return s.runLoginRequired(ctx, message)
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
