package tgclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"golang.org/x/term"
)

// DefaultFloodWaitMaxWait is the default ceiling for auto-waiting out a
// Telegram FLOOD_WAIT. It MUST stay well below the MCP client's tool-call
// timeout (Claude Desktop cancels at ~240s): auto-waiting longer is pointless
// because the client cancels the call first, turning a recoverable rate limit
// into a generic "no result received" timeout. So short, transient waits are
// absorbed transparently here, while anything longer fails fast: write tools
// render it as an actionable retry-after message (see floodWaitResult) and read
// tools surface the raw error, either beating the client's hang-until-timeout.
const DefaultFloodWaitMaxWait = 60 * time.Second

// Config holds Telegram API credentials and client tuning.
type Config struct {
	APIID   int
	APIHash string
	// FloodWaitMaxWait caps how long the flood-wait middleware will sleep on a
	// single FLOOD_WAIT before giving up and returning the error. Zero uses
	// DefaultFloodWaitMaxWait. Raising it lets the client wait out longer
	// account-level limits at the cost of blocking the in-flight call; lowering
	// it fails faster.
	FloodWaitMaxWait time.Duration
}

func (c Config) String() string {
	return fmt.Sprintf("tgclient.Config{APIID:%d APIHash:<redacted> FloodWaitMaxWait:%s}", c.APIID, c.FloodWaitMaxWait)
}

func (c Config) GoString() string { return c.String() }

// EffectiveFloodWaitMaxWait resolves the configured flood-wait ceiling,
// substituting DefaultFloodWaitMaxWait for a zero/negative value. It is the
// single source of truth for that fallback: both the middleware (CreateClient)
// and the log that explains the absorbed-vs-surfaced decision (server.Run) call
// it, so the threshold they act on can never drift apart.
func (c *Config) EffectiveFloodWaitMaxWait() time.Duration {
	if c.FloodWaitMaxWait <= 0 {
		return DefaultFloodWaitMaxWait
	}
	return c.FloodWaitMaxWait
}

// userAuthenticator implements auth.UserAuthenticator. Every line-based read
// goes through the one shared lines reader: a fresh bufio.Reader per prompt
// would buffer past the newline and strand the next answer (e.g. the 2FA
// password piped after the login code) inside a discarded buffer.
type userAuthenticator struct {
	phone string
	in    io.Reader
	lines *bufio.Reader
	out   io.Writer
}

func newUserAuthenticator(phone string, in io.Reader, out io.Writer) userAuthenticator {
	return userAuthenticator{phone: phone, in: in, lines: bufio.NewReader(in), out: out}
}

func (a userAuthenticator) readLine(what string) (string, error) {
	line, err := a.lines.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", what, err)
	}
	return strings.TrimSpace(line), nil
}

func (a userAuthenticator) Phone(_ context.Context) (string, error) {
	return a.phone, nil
}

func (a userAuthenticator) Code(_ context.Context, _ *tg.AuthSentCode) (string, error) {
	_, _ = fmt.Fprint(a.out, "Enter login code: ")
	return a.readLine("code")
}

func (a userAuthenticator) Password(_ context.Context) (string, error) {
	_, _ = fmt.Fprint(a.out, "Enter 2FA password: ")

	// Use hidden input if running in a real terminal, otherwise fall back to
	// plain line input. A terminal is line-buffered by the kernel, so the
	// shared lines reader holds nothing past the code's newline here.
	//nolint:gosec // G115: a file descriptor is always a small non-negative int; no realistic overflow.
	if inputFile, ok := a.in.(*os.File); ok && term.IsTerminal(int(inputFile.Fd())) {
		password, err := term.ReadPassword(int(inputFile.Fd())) //nolint:gosec // G115: file descriptors fit in int on supported platforms.
		_, _ = fmt.Fprintln(a.out)
		if err != nil {
			return "", fmt.Errorf("reading password: %w", err)
		}
		return string(password), nil
	}

	// Fallback for non-TTY environments (e.g., IDE, piped input).
	return a.readLine("password")
}

func (a userAuthenticator) AcceptTermsOfService(_ context.Context, _ tg.HelpTermsOfService) error {
	return nil
}

func (a userAuthenticator) SignUp(_ context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, fmt.Errorf("sign up is not supported")
}

// FloodWaitCallback is invoked when the floodwait middleware throttles a
// request. The duration is how long the middleware will sleep before retrying.
// Use this to surface throttling to the MCP client (via mcpLog at warning
// level) so users understand why a tool is slow.
type FloodWaitCallback func(ctx context.Context, duration time.Duration)

// CreateClient creates a new Telegram client with session storage and flood wait handling.
// Returns the client and a floodwait.Waiter that should wrap the client.Run() call.
// If onFloodWait is non-nil, it is invoked each time the waiter sleeps for a flood wait.
func CreateClient(cfg *Config, onFloodWait FloodWaitCallback) (*telegram.Client, *floodwait.Waiter, error) {
	storage, err := NewSessionStorage()
	if err != nil {
		return nil, nil, err
	}
	waiter := floodwait.NewWaiter().WithMaxWait(cfg.EffectiveFloodWaitMaxWait())
	if onFloodWait != nil {
		waiter = waiter.WithCallback(func(ctx context.Context, wait floodwait.FloodWait) {
			onFloodWait(ctx, wait.Duration)
		})
	}

	client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: storage,
		Middlewares:    []telegram.Middleware{waiter},
	})

	return client, waiter, nil
}

// Login performs interactive sign-in to Telegram
func Login(ctx context.Context, cfg *Config, phone string, in io.Reader, out io.Writer) error {
	client, waiter, err := CreateClient(cfg, nil)
	if err != nil {
		return fmt.Errorf("creating Telegram client: %w", err)
	}

	err = waiter.Run(ctx, func(ctx context.Context) error {
		return client.Run(ctx, func(ctx context.Context) error {
			// Check if already authorized
			status, err := client.Auth().Status(ctx)
			if err != nil {
				return fmt.Errorf("checking auth status: %w", err)
			}

			if status.Authorized {
				user, err := client.Self(ctx)
				if err != nil {
					_, _ = fmt.Fprintf(out, "Already logged in (could not fetch display name: %v)\n", err)
				} else {
					_, _ = fmt.Fprintf(out, "Already logged in as %s\n", UserName(user))
				}
				return nil
			}

			// Perform authentication
			flow := auth.NewFlow(
				newUserAuthenticator(phone, in, out),
				auth.SendCodeOptions{},
			)

			if err := flow.Run(ctx, client.Auth()); err != nil {
				return fmt.Errorf("running auth flow: %w", err)
			}

			user, err := client.Self(ctx)
			if err != nil {
				return fmt.Errorf("getting user info: %w", err)
			}

			_, _ = fmt.Fprintf(out, "Successfully logged in as %s\n", UserName(user))
			_, _ = fmt.Fprintln(out, "You can now use the mcp-telegram server.")

			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("logging in: %w", err)
	}
	return nil
}

// Logout logs out from Telegram
func Logout(ctx context.Context, cfg *Config, out io.Writer) error {
	client, waiter, err := CreateClient(cfg, nil)
	if err != nil {
		return fmt.Errorf("creating Telegram client: %w", err)
	}

	remoteErr := waiter.Run(ctx, func(ctx context.Context) error {
		return client.Run(ctx, func(ctx context.Context) error {
			if _, err := client.API().AuthLogOut(ctx); err != nil {
				return fmt.Errorf("calling auth logout: %w", err)
			}
			return nil
		})
	})
	// Delete the stored session after client.Run has returned even when the
	// remote logout failed (offline, revoked, or expired session). gotd
	// persists session state while Run is active, so deleting inside the
	// callback races with a final save that could resurrect the dead session
	// and cause a silent re-auth failure on next start.
	ss, err := NewSessionStorage()
	if err != nil {
		return errors.Join(wrapIf(remoteErr, "logging out"), fmt.Errorf("initializing session storage for local cleanup: %w", err))
	}
	cleanupErr := ss.DeleteSession()
	if remoteErr != nil || cleanupErr != nil {
		return errors.Join(wrapIf(remoteErr, "logging out"), wrapIf(cleanupErr, "deleting local session"))
	}

	_, _ = fmt.Fprintln(out, "Successfully logged out from Telegram.")
	return nil
}

func wrapIf(err error, operation string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
