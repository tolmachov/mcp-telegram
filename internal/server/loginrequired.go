package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
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

// ptrTrue returns a fresh *bool for the SDK's tri-state annotation fields.
// Mirrors the helper of the same name in internal/tools (unexported there, so
// not importable); a package-level `var openWorld = true` would hand the SDK
// an alias into mutable package state instead.
func ptrTrue() *bool {
	b := true
	return &b
}

// blockedError marks a condition our own startup callback detected — a failed
// auth check, or a session that connected fine but is not authorized. It
// carries the user-facing explanation, and it exists so the callback can
// unwind client.Run (tearing the Telegram connection down) and be recovered
// outside; see finishRun, which handles both this and the failures gotd
// reports before the callback ever runs.
type blockedError struct{ message string }

func (e *blockedError) Error() string { return e.message }

// LoginState is the outcome of the live re-check. Values are part of the
// tool's output contract, so they are constants rather than inline literals.
type LoginState string

const (
	// StateNotConfigured means api-id/api-hash were missing at startup.
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
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcp-telegram", Version: s.version}, &mcp.ServerOptions{
		Instructions: loginRequiredInstructions(reason),
		Logger:       s.logger,
	})
	mcp.AddTool(srv, &mcp.Tool{
		Name: loginRequiredTool,
		Description: "mcp-telegram is NOT connected to Telegram — every Telegram tool (sending, reading, searching, summarizing) is missing from this server for that reason. " +
			"Reason: " + reason + " " +
			"Call this to re-check the live authorization state; it reports whether a login performed elsewhere has taken effect.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptrTrue()},
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
		case s.tgConfig.APIID == 0 || s.tgConfig.APIHash == "":
			// tgConfig is frozen at process start, so this verdict cannot
			// change while we run — say so, or the model will loop on a tool
			// that keeps handing back the same answer after the user has
			// already applied the fix.
			status.State = StateNotConfigured
			status.Detail = missingCredentialsMessage + " Once they are set, " + reconnectHint
			status.FixCommand = configureCommand()
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

	// New always wires this up; the fallback keeps a Server built without it
	// (as some tests do) from panicking instead of probing.
	probe := s.authProbeFn
	if probe == nil {
		probe = s.authProbe
	}

	account, authorized, err := probe(probeCtx)
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
		// notifications anyway (see runHappy), so ask for the reconnect that
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
// is authorized, plus the display name when it is.
//
// It is serialized by probeMu. The login-required server holds no Telegram
// connection of its own, but the *session* is shared: the SDK dispatches tool
// calls concurrently, and each probe is a fresh client on the stored auth key.
// Two live clients on one key is the AUTH_KEY_DUPLICATED hazard the user pool
// goes to some length to avoid (see userpool.evictGrace), and this tool
// actively invites a concurrent `mcp-telegram login` in another terminal — so
// at least keep our own probes from stacking. The 20s ceiling on each one
// bounds how long a caller can queue behind another.
func (s *Server) authProbe(ctx context.Context) (account string, authorized bool, err error) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()

	client, waiter, err := tgclient.CreateClient(s.tgConfig, s.floodWaitLogger())
	if err != nil {
		return "", false, fmt.Errorf("constructing Telegram client: %w", err)
	}

	// checked distinguishes "the status call ran" from "client.Run returned
	// without running it", which is not visible from runErr alone: gotd
	// swallows a cancelled run into a nil error (telegram/connect.go:234), and
	// reporting that as an authoritative "not authorized" would tell the user
	// their session is dead on the strength of a probe that never happened.
	var checked bool
	runErr := waiter.Run(ctx, func(ctx context.Context) error {
		return client.Run(ctx, func(ctx context.Context) error {
			status, err := client.Auth().Status(ctx)
			if err != nil {
				return fmt.Errorf("checking auth status: %w", err)
			}
			checked = true
			authorized = status.Authorized
			// Status already carries the user object from the same
			// users.getUsers round-trip, so there is no second call to make
			// and no display-name lookup that can fail on its own.
			if status.User != nil {
				account = tgclient.UserName(status.User)
			}
			return nil
		})
	})
	switch {
	// checked outranks runErr: the auth call is the last thing the callback
	// does, so once it has answered, a late error can only come from
	// client.Run's deferred close (multierr-aggregated sub-connection
	// teardown). Discarding a completed verdict over that would report
	// check_failed for a probe that succeeded.
	case checked:
		return account, authorized, nil
	case runErr != nil && isDeadSession(runErr):
		// Telegram was reached and rejected the stored key outright. gotd
		// raises these during connection setup, so the callback never ran and
		// there is no Status to read — but the answer is not "undetermined",
		// it is "this session is dead". Reporting it as a failed check would
		// send the user chasing network problems for a session they revoked;
		// worse, it is the *only* way this route can end, so login_required
		// would otherwise be unreachable for exactly the case that motivated
		// serving this mode at all.
		return "", false, nil
	case runErr != nil:
		return "", false, fmt.Errorf("connecting to Telegram: %w", runErr)
	}
	return "", false, fmt.Errorf("the check did not complete: %w", cause(ctx))
}

// isDeadSession reports whether Telegram answered that the stored session is
// no longer usable, as opposed to being unreachable. These are the errors
// gotd itself treats as permanent (telegram/connect.go isPermanentError).
func isDeadSession(err error) bool {
	return auth.IsUnauthorized(err) ||
		tgerr.Is(err, "AUTH_KEY_UNREGISTERED", "SESSION_EXPIRED", "AUTH_KEY_DUPLICATED")
}

// cause reports why a probe ended without running, preferring the context's
// own error so a cancelled call is never mistaken for a Telegram verdict.
func cause(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("the Telegram client returned before the auth check ran")
}
