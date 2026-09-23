package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tools"
)

// loginRequiredTool is the only tool exposed while Telegram is unreachable.
// The name is deliberately a statement rather than a verb: it is what an MCP
// host prints in its server/tool list, so a glance at that list has to be
// enough to see what is wrong. Every other tool in this server is named for
// what it does; this one is named for what is broken.
const loginRequiredTool = "TelegramLoginRequired"

// authProbeTimeout bounds the live re-check the tool performs. It has to
// cover a cold connect to Telegram (DC handshake) without outliving the
// host's tool-call timeout.
const authProbeTimeout = 20 * time.Second

// LoginState is the outcome of the live re-check. Values are part of the
// tool's output contract, so they are constants rather than inline literals.
type LoginState string

const (
	// StateNotConfigured means a setting read at startup — the API
	// credentials or the summarisation provider — is missing or invalid.
	StateNotConfigured LoginState = "not_configured"
	// StateLoginRequired means Telegram was reached and the session is not
	// authorized.
	StateLoginRequired LoginState = "login_required"
	// StateCheckFailed means the probe could not determine anything.
	StateCheckFailed LoginState = "check_failed"
	// StateAuthorizedPendingReconnect means a login has taken effect since
	// this process started, so only a host-side reconnect is missing.
	StateAuthorizedPendingReconnect LoginState = "authorized_pending_reconnect"
)

// loginRequiredInstructions is what the model reads at connect time. It is
// the load-bearing part of this mode: a failed stdio connection surfaces as an
// entry the user has to drill into, and reaches the model not at all — so the
// "not authorized" state has to arrive through content the model actually
// sees.
func loginRequiredInstructions(reason string) string {
	return "mcp-telegram is running but NOT connected to Telegram, so every Telegram tool is unavailable in this session. " +
		"Reason: " + reason + " " +
		"If the user asks for anything involving Telegram, do not look for a workaround and do not report a generic failure — " +
		"tell them Telegram is not authorized and give them the fix above. " +
		"Call " + loginRequiredTool + " to re-check the live state (it detects a login completed in another terminal)."
}

// binPath is the actual path of the running binary, used to render the
// paste-ready commands in FixCommand. mcp-telegram is commonly wired into a
// host by absolute path and never put on $PATH, so a bare `mcp-telegram
// login` is a command the user cannot run. The prose messages
// (notLoggedInMessage and friends) deliberately keep the bare name: they are
// also printed on a TTY, where $PATH did resolve the binary.
func binPath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "mcp-telegram"
}

func loginCommand() string {
	return binPath() + " login --phone <+countrycode…>"
}

func configureCommand() string {
	bin := binPath()
	return bin + " config set api-id <id> && " + bin + " config set api-hash <hash>"
}

// reconnectHint names the host-side action, generically first because the
// exact wording and the server's registered name differ per host and per
// user's config.
const reconnectHint = "reconnect this MCP server to load the Telegram tools (in Claude Code: /mcp → select this server → Reconnect)."

// LoginRequiredStatus is the result of the TelegramLoginRequired tool: a live
// re-check rather than a replay of the startup reason, so the model can tell
// "still logged out" from "logged in since, needs a reconnect".
type LoginRequiredStatus struct {
	// TelegramToolsAvailable is always false. It states the one thing that is
	// certain in this mode and that no other field expresses: whatever the
	// session's status, this server process has no Telegram tools loaded.
	TelegramToolsAvailable bool `json:"telegram_tools_available" jsonschema:"always false — this server process exposes no Telegram tools regardless of session state"`
	// Authorized is derived from State, never set independently, so the two
	// cannot contradict each other. False also covers "not determined" (see
	// State); it is not a claim that the session was checked and found dead.
	Authorized bool       `json:"authorized" jsonschema:"true only when a live check succeeded and found the session authorized; false also covers not-checked — read state for which"`
	State      LoginState `json:"state" jsonschema:"one of not_configured, login_required, check_failed, authorized_pending_reconnect"`
	Detail     string     `json:"detail" jsonschema:"human-readable explanation of the current state"`
	FixCommand string     `json:"fix_command,omitempty" jsonschema:"best-known next command to run, when one applies"`
	Account    string     `json:"account,omitempty" jsonschema:"display name of the signed-in account, when authorized"`
	// StartupReason is the condition this process started with, omitted when
	// the live re-check just repeated it.
	StartupReason string `json:"startup_reason,omitempty" jsonschema:"the condition this server started with, when it differs from detail"`
}

// runLoginRequired serves a single-tool MCP server over stdio instead of
// refusing to start.
//
// The alternative — answering the initialize request with a JSON-RPC error —
// is the obvious reading of the MCP lifecycle spec and it is what this code
// used to do, but it does not survive contact with real hosts: the message
// arrives intact and is then rendered as a bare failure entry whose text is
// only reachable by drilling in (observed in Claude Code's /mcp list), which
// is indistinguishable from a crashed binary or a bad path. Coming up with
// one loudly-named tool and instructions that say what is wrong puts the
// diagnosis in front of both the user (server/tool list) and the model
// (instructions), which is the only channel a stdio server actually has.
func (s *Server) runLoginRequired(ctx context.Context, reason string) error {
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcp-telegram", Version: s.opts.Version}, &mcp.ServerOptions{
		Instructions: loginRequiredInstructions(reason),
		Logger:       s.logger,
	})
	tools.AddTool(srv, &mcp.Tool{
		Name: loginRequiredTool,
		Description: "mcp-telegram is NOT connected to Telegram — every Telegram tool (sending, reading, searching, summarizing) is missing from this server for that reason. " +
			"Reason: " + reason + " " +
			"Call this to re-check the live authorization state; it reports whether a login performed elsewhere has taken effect.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, s.loginRequiredHandler(reason))

	s.logger.Warn("serving in login-required mode",
		"reason", reason,
		"tool", loginRequiredTool,
		"detail", "Telegram tools are not registered; the server is up only to report this state",
	)

	if err := srv.Run(ctx, s.stdioTransport()); err != nil {
		if errors.Is(err, context.Canceled) {
			// Host shutdown, not a failure. Returning it would exit non-zero
			// and read as a crash to whatever supervises the process.
			s.logger.Info("login-required server stopped", "reason", "context canceled")
			return nil
		}
		return fmt.Errorf("running login-required server: %w", err)
	}
	return nil
}

func (s *Server) loginRequiredHandler(reason string) func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, *LoginRequiredStatus, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, *LoginRequiredStatus, error) {
		status := &LoginRequiredStatus{}

		switch {
		// The configuration is frozen at process start, so these verdicts
		// cannot change while we run — say so, or the model will loop on a
		// tool that keeps handing back the same answer after the user has
		// already applied the fix.
		case s.opts.Config.APIID == 0 || s.opts.Config.APIHash == "":
			status.State = StateNotConfigured
			status.Detail = missingCredentialsMessage + " Once they are set, " + reconnectHint
			status.FixCommand = configureCommand()
		case s.summarizeErr != nil:
			status.State = StateNotConfigured
			status.Detail = summarizeMisconfiguredMessage(s.summarizeErr)
		default:
			s.fillProbedStatus(ctx, status)
		}

		// Only worth repeating when the live detail does not already carry it;
		// otherwise the payload the model reads is the same paragraph twice.
		if !strings.Contains(status.Detail, reason) {
			status.StartupReason = reason
		}
		// Derived, never assigned alongside State, so the two cannot drift.
		status.Authorized = status.State == StateAuthorizedPendingReconnect
		return nil, status, nil
	}
}

// fillProbedStatus runs the live re-check and records its outcome.
func (s *Server) fillProbedStatus(ctx context.Context, status *LoginRequiredStatus) {
	probeCtx, cancel := context.WithTimeout(ctx, authProbeTimeout)
	defer cancel()

	account, authorized, err := s.authProbeFn(probeCtx)
	if err != nil && probeCtx.Err() != nil && ctx.Err() == nil {
		// Our own ceiling fired, not one the host imposed on the tool call —
		// only then is naming authProbeTimeout accurate.
		err = fmt.Errorf("timed out after %s: %w", authProbeTimeout, err)
	}
	switch {
	case err != nil:
		// Deliberately vague about the cause: this bucket collects network
		// failures, session-storage errors, and a denied Keychain prompt
		// alike, and guessing wrong sends the user chasing the wrong fix. The
		// raw error carries the truth.
		status.State = StateCheckFailed
		status.Detail = fmt.Sprintf("Could not determine the Telegram session state: %v. Retry; if it persists, check network connectivity and access to the session store.", err)
		status.FixCommand = loginCommand()
		s.logger.Warn("login-required re-check failed", "err", err)
	case authorized:
		// The user logged in from another terminal while this process was
		// already up. Loading the Telegram tools means building the assembly
		// inside a live client.Run scope and swapping it into the running
		// session; the variants proxy could not forward the resulting change
		// notifications anyway (see serveAssembly), so ask for the reconnect that
		// rebuilds cleanly.
		status.State = StateAuthorizedPendingReconnect
		status.Account = account
		status.Detail = "Telegram is authorized now" + accountSuffix(account) + ", but this server process started without it — " + reconnectHint
		s.logger.Info("login-required re-check found an authorized session", "account", account, "detail", "a host-side reconnect will load the Telegram tools")
	default:
		status.State = StateLoginRequired
		status.Detail = notLoggedInMessage
		status.FixCommand = loginCommand()
		s.logger.Info("login-required re-check: still not authorized")
	}
}

func accountSuffix(account string) string {
	if account == "" {
		return ""
	}
	return " as " + account
}

// authProbe connects to Telegram on the stored session and reports whether it
// is authorized, plus the display name when it is. It goes through
// startLocalClient, so it answers exactly as startup would: a session
// Telegram refused — whether through the auth check or already in the connect
// phase, where there is no Status to read — is the verdict "not authorized",
// not a failed check, while a cancelled or incomplete probe stays an error
// and is never reported as a dead session.
//
// It is serialised by probeMu. The login-required server holds no Telegram
// connection of its own, but the *session* is shared: the SDK dispatches tool
// calls concurrently, and each probe is a fresh client on the stored auth key.
// Two live clients on one key is the AUTH_KEY_DUPLICATED hazard the user pool
// goes to some length to avoid (see userPoolEvictGrace), and this tool
// actively invites a concurrent `mcp-telegram login` in another terminal — so
// at least keep our own probes from stacking. The authProbeTimeout ceiling on
// each one bounds how long a caller can queue behind another.
func (s *Server) authProbe(ctx context.Context) (account string, authorized bool, err error) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()

	return probeVerdict(s.startLocalClient(ctx))
}

// probeVerdict turns a startLocalClient outcome into the re-check's answer.
func probeVerdict(running *tgclient.Running, err error) (account string, authorized bool, _ error) {
	switch {
	case errors.Is(err, tgclient.ErrSessionUnauthorized):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	account = tgclient.UserName(running.Self())
	running.Close()
	return account, true, nil
}
