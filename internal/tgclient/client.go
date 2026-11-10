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

	"github.com/gotd/log"
	"github.com/gotd/log/logslog"
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
	// FloodWaitMaxWait caps how long the Telegram calls of one tool call wait
	// in all on the waits Telegram tells them to take (see WithWaitBudget): a
	// wait that would take them past it is returned as the call's error
	// instead; the default lives on --flood-wait-max-seconds. Raising it lets
	// the client wait out longer account-level limits at the cost of holding
	// the tool call that waits; lowering it fails faster.
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

func (a userAuthenticator) readLine(ctx context.Context, what string) (string, error) {
	line, err := readInput(ctx, func() ([]byte, error) { return a.lines.ReadBytes('\n') })
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", what, err)
	}
	return strings.TrimSpace(string(line)), nil
}

func (a userAuthenticator) Phone(_ context.Context) (string, error) {
	return a.phone, nil
}

func (a userAuthenticator) Code(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
	_, _ = fmt.Fprint(a.out, "Enter login code: ")
	return a.readLine(ctx, "code")
}

func (a userAuthenticator) Password(ctx context.Context) (string, error) {
	//nolint:gosec // G115: file descriptors fit in int on supported platforms.
	if inputFile, ok := a.in.(*os.File); ok && term.IsTerminal(int(inputFile.Fd())) {
		return a.terminalPassword(ctx, inputFile)
	}
	_, _ = fmt.Fprint(a.out, "Enter 2FA password: ")
	return a.readLine(ctx, "password")
}

func (a userAuthenticator) terminalPassword(ctx context.Context, in *os.File) (password string, err error) {
	fd := int(in.Fd()) //nolint:gosec // G115: file descriptors fit in int on supported platforms.
	state, err := term.MakeRaw(fd)
	if err != nil {
		return "", fmt.Errorf("preparing password terminal: %w", err)
	}
	defer func() {
		err = errors.Join(err, wrapIf(term.Restore(fd, state), "restoring password terminal"))
	}()

	// Only the read runs in the background: terminal setup and restoration
	// must finish before returning, including on cancellation. Terminal handles
	// Ctrl+C itself in raw mode and never echoes the password.
	terminal := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{contextReader{ctx: ctx, in: in}, a.out}, "")
	password, err = terminal.ReadPassword("Enter 2FA password: ")
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}
	return password, nil
}

func (a userAuthenticator) AcceptTermsOfService(_ context.Context, _ tg.HelpTermsOfService) error {
	return nil
}

func (a userAuthenticator) SignUp(_ context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, fmt.Errorf("sign up is not supported")
}

// newClient builds a gotd client over storage from opts, with the flood-wait
// middleware (floodWait), which logs every wait through logger, in front of
// opts.Middlewares.
func newClient(ctx context.Context, cfg *Config, storage session.Storage, logger *slog.Logger, opts telegram.Options) *telegram.Client {
	opts = withConnectDefaults(ctx, storage, opts)
	opts.SessionStorage = storage
	opts.Middlewares = append([]telegram.Middleware{floodWait(cfg.FloodWaitMaxWait, logger)}, opts.Middlewares...)
	return telegram.NewClient(cfg.APIID, cfg.APIHash, opts)
}

// Login performs interactive sign-in to Telegram
func Login(ctx context.Context, cfg *Config, phone string, in io.Reader, out io.Writer) error {
	storage, err := NewSessionStorage()
	if err != nil {
		return fmt.Errorf("opening session storage: %w", err)
	}
	client := newClient(ctx, cfg, storage, slog.Default(), telegram.Options{})

	err = client.Run(ctx, func(ctx context.Context) error {
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
	client := newClient(ctx, cfg, storage, slog.Default(), telegram.Options{})

	remoteErr := client.Run(ctx, func(ctx context.Context) error {
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
