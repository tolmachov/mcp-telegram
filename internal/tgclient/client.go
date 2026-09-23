package tgclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/gotd/contrib/middleware/floodwait"
	"github.com/gotd/log"
	"github.com/gotd/log/logslog"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"golang.org/x/term"
)

// Config holds Telegram API credentials and client tuning.
type Config struct {
	APIID   int
	APIHash string
	// FloodWaitMaxWait caps how long the flood-wait middleware will sleep on a
	// single FLOOD_WAIT before giving up and returning the error; the default
	// lives on --flood-wait-max-seconds. Raising it lets the client wait out
	// longer account-level limits at the cost of blocking the in-flight call;
	// lowering it fails faster.
	FloodWaitMaxWait time.Duration
}

func (c Config) String() string {
	return fmt.Sprintf("tgclient.Config{APIID:%d APIHash:<redacted> FloodWaitMaxWait:%s}", c.APIID, c.FloodWaitMaxWait)
}

func (c Config) GoString() string { return c.String() }

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

// newClient builds a gotd client over storage from opts, with the flood-wait
// middleware in front of opts.Middlewares. The returned run drives the client
// and must wrap every use of it: the middleware only waits out a FLOOD_WAIT
// inside it. If onFloodWait is non-nil, it is invoked each time the middleware
// sleeps for a flood wait.
func newClient(cfg *Config, storage session.Storage, onFloodWait FloodWaitCallback, opts telegram.Options) (*telegram.Client, func(context.Context, func(context.Context) error) error) {
	waiter := floodwait.NewWaiter().WithMaxWait(cfg.FloodWaitMaxWait)
	if onFloodWait != nil {
		waiter = waiter.WithCallback(func(ctx context.Context, wait floodwait.FloodWait) {
			onFloodWait(ctx, wait.Duration)
		})
	}
	opts.SessionStorage = storage
	opts.Middlewares = append([]telegram.Middleware{waiter}, opts.Middlewares...)
	client := telegram.NewClient(cfg.APIID, cfg.APIHash, opts)
	run := func(ctx context.Context, f func(context.Context) error) error {
		return waiter.Run(ctx, func(ctx context.Context) error {
			return client.Run(ctx, f)
		})
	}
	return client, run
}

// Login performs interactive sign-in to Telegram
func Login(ctx context.Context, cfg *Config, phone string, in io.Reader, out io.Writer) error {
	storage, err := NewSessionStorage()
	if err != nil {
		return fmt.Errorf("opening session storage: %w", err)
	}
	client, run := newClient(cfg, storage, nil, telegram.Options{})

	err = run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("checking auth status: %w", err)
		}
		if status.Authorized {
			_, _ = fmt.Fprintf(out, "Already logged in as %s\n", UserName(status.User))
			return nil
		}

		flow := auth.NewFlow(
			newUserAuthenticator(phone, in, out),
			auth.SendCodeOptions{},
		)
		if err := flow.Run(ctx, client.Auth()); err != nil {
			return fmt.Errorf("running auth flow: %w", err)
		}

		// The flow does not hand back the signed-in user, so this is the one
		// place that has to ask for it.
		user, err := client.Self(ctx)
		if err != nil {
			return fmt.Errorf("getting user info: %w", err)
		}

		_, _ = fmt.Fprintf(out, "Successfully logged in as %s\n", UserName(user))
		_, _ = fmt.Fprintln(out, "You can now use the mcp-telegram server.")

		return nil
	})
	if err != nil {
		return fmt.Errorf("logging in: %w", err)
	}
	return nil
}

// Logout logs out from Telegram
func Logout(ctx context.Context, cfg *Config, out io.Writer) error {
	storage, err := NewSessionStorage()
	if err != nil {
		return fmt.Errorf("opening session storage: %w", err)
	}
	client, run := newClient(cfg, storage, nil, telegram.Options{})

	remoteErr := run(ctx, func(ctx context.Context) error {
		if _, err := client.API().AuthLogOut(ctx); err != nil {
			return fmt.Errorf("calling auth logout: %w", err)
		}
		return nil
	})
	// Delete the stored session after the client has stopped even when the
	// remote logout failed (offline, revoked, or expired session). gotd
	// persists session state while Run is active, so deleting inside the
	// callback races with a final save that could resurrect the dead session
	// and cause a silent re-auth failure on next start.
	cleanupErr := storage.DeleteSession()
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

// refusalWatch is the one place a client learns that Telegram may have
// declared its session dead: every call's reply passes through it, and a
// refusal (isSessionRefusal) is handed to confirm together with the invoker
// below the watch, so the client's owner can check it on the home DC whichever
// tool, resource or background poller happened to make the call. The call
// returns what confirm returns.
func refusalWatch(confirm func(ctx context.Context, next tg.Invoker, refusal error) error) telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			err := next.Invoke(ctx, input, output)
			if err != nil && isSessionRefusal(err) {
				return confirm(ctx, next, err)
			}
			return err //nolint:wrapcheck // a middleware passes Telegram's reply through unchanged.
		}
	})
}

// gotdLogger hands gotd the caller's logger, floored at Warn: gotd logs every
// connection's lifecycle at Info and every call at Debug, which would bury the
// server's own records, while its warnings (a permanent connection failure, a
// dropped key) are what an operator needs.
func gotdLogger(logger *slog.Logger) log.Logger {
	return logslog.New(slog.New(warnFloor{logger.Handler()}).With("subsystem", "gotd"))
}

// warnFloor passes on only records at Warn and above.
type warnFloor struct{ slog.Handler }

func (h warnFloor) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn && h.Handler.Enabled(ctx, level)
}

func (h warnFloor) WithAttrs(attrs []slog.Attr) slog.Handler {
	return warnFloor{h.Handler.WithAttrs(attrs)}
}

func (h warnFloor) WithGroup(name string) slog.Handler {
	return warnFloor{h.Handler.WithGroup(name)}
}
