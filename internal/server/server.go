package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
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

// Options configures New: the resolved CLI settings, the process's standard
// streams and, for the http transport, the session store the caller builds.
// New validates the combination and builds the summariser; a summarisation
// misconfiguration only disables SummarizeChat (see Summarize).
type Options struct {
	Config       *tgclient.Config
	Version      string
	AllowedPaths []string // --allowed-paths; empty → the OS backup directory (see backupAllowedPaths)
	// Summarize configures SummarizeChat; New builds the summariser from it.
	// Summarisation is optional, so a misconfiguration is logged at startup
	// and reported by SummarizeChat alone; every other tool works.
	Summarize      summarize.Config
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

	// summarizer serves SummarizeChat; summarizeErr is why opts.Summarize
	// could not build one, which SummarizeChat reports (see
	// summarizeUnavailable).
	summarizer   *summarize.Summarizer
	summarizeErr error

	// openLocalSession opens the stored session of the single local account
	// (the Keychain on darwin, the state-directory file elsewhere). New points
	// it at tgclient.NewSessionStorage; only tests replace it, to start a
	// client without touching the real session.
	openLocalSession func() (session.Storage, error)
	// connectLocal starts the client Run serves over stdio. New points it at
	// startLocalClient; only tests replace it, to serve on a fake client.
	connectLocal func(context.Context) (telegramClient, error)

	// authProbeFn is the live authorization re-check the login-required tool
	// performs, injectable so the tool's states can be exercised without a
	// Telegram connection (and, on darwin, without a Keychain prompt). New
	// points it at Server.authProbe; only tests replace it.
	authProbeFn func(context.Context) (account string, authorized bool, err error)
	// probeMu serialises authProbe — see its doc comment for why concurrent
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
	srv.openLocalSession = func() (session.Storage, error) { return tgclient.NewSessionStorage() }
	srv.connectLocal = func(ctx context.Context) (telegramClient, error) {
		running, err := srv.startLocalClient(ctx)
		if err != nil {
			return nil, err
		}
		return running, nil
	}
	srv.summarizer, srv.summarizeErr = summarize.New(opts.Summarize)
	if srv.summarizeErr != nil {
		logger.Warn("summarisation is misconfigured; SummarizeChat reports it and every other tool works", "err", srv.summarizeErr)
	}
	srv.authProbeFn = srv.authProbe
	return srv, nil
}

// summarizeUnavailable is what SummarizeChat fails with when summarisation is
// misconfigured, or nil: the startup error and its fix. The settings are read
// once, at startup, so the fix ends with the process reading them again.
func (s *Server) summarizeUnavailable() error {
	if s.summarizeErr == nil {
		return nil
	}
	reload := "reconnect this MCP server so it reads them again (in Claude Code: /mcp → select this server → Reconnect)"
	if s.opts.Transport == TransportHTTP {
		reload = "restart the server so it reads them again"
	}
	return fmt.Errorf("summarisation is not available, as its settings were invalid at startup: %w. Fix the setting named — a flag or MCP_SUMMARIZE_* environment variable in this server's configuration, or an API key stored with `mcp-telegram config set` — then %s", s.summarizeErr, reload)
}

// Run starts the MCP server on the configured transport (stdio, or streamable
// HTTP behind the embedded OAuth server). When the server cannot serve at
// all — missing credentials, an unusable session store, a connect-phase failure, a failed auth check, or a
// session that is simply not authorized — the stdio path comes up in
// login-required mode instead of failing: a server exposing one loudly-named
// tool that reports the problem, plus instructions that say the same thing to
// the model. See runLoginRequired for why that beats failing the connection.
//
// Over HTTP only the configuration conditions can arise, because the per-user
// clients are connected lazily by the pool; with no MCP peer to tell, they
// fail the process. A host that cancels ctx during startup gets a quiet nil.
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

	client, err := s.connectLocal(ctx)
	if err != nil {
		if ctx.Err() != nil {
			// The host shut down while the client was still starting:
			// nothing failed, and nobody is left to read a login-required
			// answer.
			return nil
		}
		if errors.Is(err, tgclient.ErrSessionUnauthorized) {
			s.logger.Warn("no Telegram access", "reason", "not authorized; login required", "err", err)
		} else {
			s.logger.Error("no Telegram access", "reason", "could not start the Telegram client", "err", err)
		}
		return s.startBlocked(ctx, blockedReason(err))
	}
	return s.serveAssembly(ctx, client)
}

// startLocalClient connects the single local account on its stored session
// through the same StartClient the HTTP pool uses per user, so stdio startup
// and the login-required re-check share one "authorized / unauthorized /
// failed" answer.
func (s *Server) startLocalClient(ctx context.Context) (*tgclient.Running, error) {
	storage, err := s.openLocalSession()
	if err != nil {
		return nil, fmt.Errorf("opening session storage: %w", err)
	}
	running, err := tgclient.StartClient(ctx, s.opts.Config, storage, s.logger, s.floodWaitLogger())
	if err != nil {
		return nil, fmt.Errorf("starting Telegram client: %w", err)
	}
	return running, nil
}

// blockedReason is the login-required explanation for a failed
// startLocalClient. Only a session Telegram refused gets the login advice
// outright; anything else may be a network problem as much as a dead or
// corrupt session, so the raw error leads.
func blockedReason(err error) string {
	if errors.Is(err, tgclient.ErrSessionUnauthorized) {
		return notLoggedInMessage
	}
	return fmt.Sprintf("mcp-telegram: could not connect to Telegram: %v. If this is a network problem, verify connectivity and retry; if the stored session was revoked or is corrupt, `mcp-telegram logout` followed by `mcp-telegram login` recovers it.", err)
}

// serveAssembly serves the Telegram tools over stdio on a connected client
// until the host disconnects (closes stdin) or ctx is cancelled, then
// disconnects the client.
//
// A client that stops on its own — Telegram refusing the session mid-run, or
// a permanent transport failure — does not end the session: the host stays
// connected and every tool call answers with why and how to recover
// (clientDownMiddleware), which is the login-required state reached
// mid-session. Swapping in the login-required server itself is not possible:
// it would hand the host's stdin from one MCP session to the next, and the
// SDK's stdio connection keeps reading ahead into a buffer it drops on close,
// losing whatever the host sent in between.
func (s *Server) serveAssembly(ctx context.Context, client telegramClient) error {
	asm, err := s.buildAssembly(ctx, client, s.logger)
	if err != nil {
		return err
	}
	serveErr := asm.run(ctx, s.stdioTransport())
	if ctx.Err() != nil {
		// The host cancelled ctx: shutdown, not a failure.
		serveErr = nil
	}
	if err := errors.Join(serveErr, asm.Close()); err != nil {
		return fmt.Errorf("running MCP server: %w", err)
	}
	return nil
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
// client, plus the pinned-chat watcher mirroring its resource set: either the
// SEP-2053 variants proxy or a single pinned-variant server. The stdio path
// builds exactly one; the HTTP auth mode builds one per authenticated user.
// Callers serve it through run (stdio) or ServeHTTP (http) and never pick the
// form themselves. The assembly owns its client: Close disconnects it.
type assembly struct {
	variants *variants.Server // non-nil when exposing all variants
	single   *mcp.Server      // non-nil when --variant pins one
	client   telegramClient
	// handler serves the assembly over streamable HTTP; set for the http
	// transport only.
	handler http.Handler

	// end ends the assembly's lifetime: the pinned-chat watcher and any
	// chat-list load or peer resolve still running for it. watchDone closes
	// once the watcher has exited.
	end       context.CancelFunc
	watchDone <-chan struct{}
	logger    *slog.Logger
}

// run serves the assembly on one connection over t until ctx ends or the
// peer disconnects.
func (a *assembly) run(ctx context.Context, t mcp.Transport) error {
	if a.variants == nil {
		if err := a.single.Run(ctx, t); err != nil {
			return fmt.Errorf("pinned-variant server: %w", err)
		}
		return nil
	}
	// The variants proxy cannot forward async resources/list_changed
	// notifications (they fire from the watcher goroutine on a background
	// context with no front session to redirect to — a documented library
	// limitation). Pinned resources are still exposed on every variant and
	// refreshed by the poller, so clients see the updated set on their next
	// resources/list; only proactive change-notifications are unavailable.
	// Pin a single --variant to restore live notifications.
	a.logger.Info("multi-variant mode: pinned-chat resources are exposed on every variant and refreshed by one poller, but live resources/list_changed notifications are not delivered through the variants proxy; pin a single --variant for live updates")
	if err := a.variants.Run(ctx, t); err != nil {
		return fmt.Errorf("variants proxy: %w", err)
	}
	return nil
}

// httpHandler builds the assembly's streamable HTTP handler.
func (a *assembly) httpHandler() http.Handler {
	if a.variants == nil {
		srv := a.single
		return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, streamableHTTPOptions())
	}
	return variants.NewStreamableHTTPHandler(a.variants, streamableHTTPOptions())
}

// ServeHTTP serves one request of the assembly over streamable HTTP.
func (a *assembly) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.handler.ServeHTTP(w, r)
}

// Err is nil while the assembly can serve and, once its Telegram client has
// stopped, why: wrapping tgclient.ErrSessionUnauthorized when Telegram
// refused the session.
func (a *assembly) Err() error { return a.client.Err() }

// telegramClient is the running Telegram client an assembly serves on;
// *tgclient.Running is the production one.
type telegramClient interface {
	API() *tg.Client
	// Err is nil while the client serves and why it stopped once it has.
	Err() error
	// Done is closed once the client has stopped.
	Done() <-chan struct{}
	// Close disconnects the client and waits for it to stop.
	Close()
}

// buildAssembly constructs handlers, resources, prompts, and the MCP
// server(s) for one Telegram client. The assembly lives on a child of ctx —
// its pinned-chat watcher and the loads its chat-list cache and peer resolver
// share between calls run there — which ends on Close or once the client
// stops.
//
// The assembly takes ownership of client: the caller must Close the
// assembly, which disconnects it. When no assembly comes out — an error or a
// panic in the wiring — buildAssembly disconnects the client itself.
func (s *Server) buildAssembly(ctx context.Context, client telegramClient, logger *slog.Logger) (asm *assembly, err error) {
	// The assembly gets a lifetime of its own because the stdio host
	// normally shuts us down by closing stdin, which ends the serve loop
	// while ctx stays live; Close ends it either way.
	life, end := context.WithCancel(ctx)
	defer func() {
		if asm == nil {
			end()
			client.Close()
		}
	}()
	api := client.API()
	// One chat-list cache shared by GetChats, SearchChats, the chats
	// resource and completion, so none of them re-paginates every dialog on
	// its own.
	chatsCache := tgdata.NewChatsCache(life, func(ctx context.Context, onProgress tgdata.ProgressFunc) (*tgdata.ChatsList, error) {
		return tgdata.GetChats(ctx, api, onProgress)
	})

	impl := &mcp.Implementation{Name: "mcp-telegram", Version: s.opts.Version}
	serverOpts := &mcp.ServerOptions{
		Instructions: happyInstructions,
		Logger:       logger,
		// Suggest chat titles/usernames/ids for prompt arguments and the
		// chat resource template as the user types.
		CompletionHandler: completion.Handler(chatsCache),
	}

	// One peer resolver — cache and stale-hash retry — shared by every tool,
	// resource and the message provider.
	peers := tgclient.NewResolver(life, api)
	// The message provider owns the rate limiter its fetches wait on. The
	// RPS ceiling is configurable (--tg-rate-limit-rps) so operators can
	// loosen it when fetches bottleneck on it. Raising it too high will trip
	// Telegram's FLOOD_WAIT, which the tgclient waiter wrapper reports via
	// onFloodWait.
	msgProvider := messages.NewProvider(peers, s.opts.TGRateLimitRPS)

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
		srv.AddReceivingMiddleware(s.clientDownMiddleware(client))
	}

	built := &assembly{client: client, end: end, logger: logger}
	// pinnedServers are the inner servers the pinned-chat watcher mirrors its
	// resource set onto.
	var pinnedServers []*mcp.Server
	if s.opts.Variant == "" {
		built.variants, pinnedServers = buildVariantsServer(impl, serverOpts, fullHandlers, researchHandlers, wire, logger)
	} else {
		d, ok := defForVariant(s.opts.Variant)
		if !ok {
			// Unreachable: New rejects unknown non-empty variants, and Server is
			// only constructible through New. Fail loudly rather than silently
			// falling back to the zero-value mode if that invariant is ever broken.
			return nil, fmt.Errorf("buildAssembly: variant %q not found in table (should have been rejected by New)", s.opts.Variant)
		}
		built.single = newInnerForMode(impl, serverOpts, fullHandlers, researchHandlers, d.mode, wire, logger)
		pinnedServers = []*mcp.Server{built.single}
	}
	if s.opts.Transport == TransportHTTP {
		built.handler = built.httpHandler()
	}

	// The SDK has no BeforeListResources hook, so the pinned-chat set is
	// refreshed by a periodic poller (default 30s, --pinned-refresh-seconds).
	// list_changed only fires when the set actually changes, so the ticker is
	// safe to run on a short interval.
	pinnedProvider := resources.NewPinnedChatsProvider(api, msgProvider, logger, pinnedServers...)
	built.watchDone = pinnedProvider.WatchInBackground(life, s.opts.PinnedRefresh)
	// A client that stops on its own ends the assembly's lifetime too: there
	// is nothing left for the watcher to poll or a load to call. Close ends
	// the lifetime before it stops the client, so a stop found with the
	// lifetime already over is the assembly's own teardown.
	go func() {
		select {
		case <-client.Done():
			if life.Err() == nil {
				logger.Warn("Telegram client stopped; tool calls and resource reads now answer with the reason", "err", client.Err())
				end()
			}
		case <-life.Done():
		}
	}()
	return built, nil
}

// clientDownMiddleware tells tool calls and resource reads why the assembly's
// Telegram client has stopped. A call arriving afterwards never reaches
// Telegram and is answered with clientDownText alone. A call that ran and
// failed while the client stopped under it keeps its own outcome — the real
// cause and any note, such as the partial file a backup saved — with
// clientDownText appended.
//
// Completion is left out on purpose: its suggestions go to the user's input
// box, not to the model, and it answers an empty list on any failure by
// design, so it has no failure to append to and no reader for the reason.
// It does not reach the stopped client either: the client's stop ends the
// assembly's lifetime, which fails the chat-list loads completion reads.
func (s *Server) clientDownMiddleware(client telegramClient) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != methodCallTool && method != methodReadResource {
				return next(ctx, method, req)
			}
			if down := client.Err(); down != nil {
				return clientDownResult(method, s.clientDownText(down))
			}
			res, err := next(ctx, method, req)
			if down := client.Err(); down != nil && callFailed(res, err) {
				return withClientDown(res, err, s.clientDownText(down))
			}
			return res, err
		}
	}
}

// callFailed reports whether a tools/call or resources/read outcome is a
// failure: a protocol error, or a tool result flagged IsError.
func callFailed(res mcp.Result, err error) bool {
	if err != nil {
		return true
	}
	tr, ok := res.(*mcp.CallToolResult)
	return ok && tr.IsError
}

// clientDownResult delivers text as the outcome of method: a tool error the
// model reads, or a resource-read error.
func clientDownResult(method, text string) (mcp.Result, error) {
	if method == methodCallTool {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
	}
	return nil, errors.New(text)
}

// withClientDown appends text to a failed outcome (see callFailed): to the
// error of a failed call, or as a further text block of a tool error.
func withClientDown(res mcp.Result, err error, text string) (mcp.Result, error) {
	if err != nil {
		return res, fmt.Errorf("%w %s", err, text)
	}
	tr := *res.(*mcp.CallToolResult)
	tr.Content = append(slices.Clip(tr.Content), &mcp.TextContent{Text: text})
	return &tr, nil
}

// clientDownText says why an assembly's Telegram client stopped and what
// recovers it on this transport. Over HTTP the pool acts on the next request:
// a refused session is deleted and the request answered 401, which sends the
// MCP client back through the QR login, and any other stop is rebuilt. Over
// stdio nothing restarts the client inside this process, so the fix involves
// the host reconnecting the server.
func (s *Server) clientDownText(err error) string {
	refused := errors.Is(err, tgclient.ErrSessionUnauthorized)
	switch {
	case s.opts.Transport == TransportHTTP && refused:
		return fmt.Sprintf("Telegram refused this account's session (%v): it was logged out, revoked or expired. The server has stopped using it, and the client's next request is answered with an authorisation error that sends it through the Telegram QR login again.", err)
	case s.opts.Transport == TransportHTTP:
		return fmt.Sprintf("The Telegram connection for this account stopped (%v). Retry the call: the next request reconnects it.", err)
	case refused:
		return fmt.Sprintf("Telegram refused this server's session (%v), so every Telegram tool is unavailable until the session is replaced. %s", err, notLoggedInMessage)
	default:
		return fmt.Sprintf("mcp-telegram's Telegram client stopped (%v), so every Telegram tool is unavailable until this MCP server is reconnected. If it stops again, the stored session may be corrupt: `mcp-telegram logout` followed by `mcp-telegram login` recovers it.", err)
	}
}

// pinnedWatchExitTimeout bounds how long Close waits for the pinned-chat
// watcher to exit.
const pinnedWatchExitTimeout = 5 * time.Second

// Close ends the assembly's lifetime — stopping the pinned-chat watcher and
// failing any shared chat-list load or peer resolve still running — closes
// the variants proxy and, last, disconnects the Telegram client, so nothing
// of the assembly still runs on it. It waits for the watcher to exit so its
// goroutine cannot race with server teardown while mid-way through
// AddResource/RemoveResources. The cancel stops it deterministically; the timeout only guards against a genuinely
// wedged provider (e.g. blocked in a Telegram call) holding up shutdown
// indefinitely. If it ever fires we are abandoning a live goroutine that will
// then touch a torn-down server — a real correctness hazard, so it logs at
// Error, not Warn.
func (a *assembly) Close() error {
	a.end()
	select {
	case <-a.watchDone:
	case <-time.After(pinnedWatchExitTimeout):
		a.logger.Error("pinned-chat watcher did not exit in time; abandoning", "timeout", pinnedWatchExitTimeout)
	}
	var err error
	if a.variants != nil {
		if closeErr := a.variants.Close(); closeErr != nil {
			err = fmt.Errorf("closing variants proxy: %w", closeErr)
		}
	}
	a.client.Close()
	return err
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
		tools.NewChatSummarizeHandler(msgProvider, s.summarizer, s.summarizeUnavailable()),
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
