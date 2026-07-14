package tgclient

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth/qrlogin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// QRState is the observable phase of an in-flight QR login.
type QRState int

const (
	QRWaiting        QRState = iota // QR shown, waiting for a scan
	QRPasswordNeeded                // account has 2FA; waiting for the password
	QRDone                          // logged in, session bytes final
	QRFailed                        // terminal failure (incl. abort/expiry)
)

// QRUser identifies the account that accepted the QR login.
type QRUser struct {
	ID       tgid.UserID
	Username string
}

// qrPasswordAttempts bounds wrong-2FA-password retries before the flow fails.
const qrPasswordAttempts = 3

// QRFlow is one Telegram QR login running on a background goroutine with its
// own short-lived client and an in-memory session. The session deliberately
// never touches disk here: the caller decides where the final bytes go
// (SessionData) only after checking who actually scanned the code.
//
// State transitions: Waiting → (PasswordNeeded →) Done | Failed. The done
// channel closes only after the underlying client.Run has returned, i.e.
// when the session bytes are final and no bootstrap connection remains —
// callers may hand the session to a long-lived client without risking two
// concurrent users of one auth key.
type QRFlow struct {
	mu          sync.Mutex
	tokenURL    string
	state       QRState
	user        QRUser
	sessionData []byte
	err         error

	passwordCh chan string
	cancel     context.CancelFunc
	done       chan struct{}
}

// StartQRLogin exports a fresh QR login token and runs the login state
// machine in the background. ctx contributes values only — cancellation is
// stripped via context.WithoutCancel before the goroutine starts, so the flow
// is fully detached and must be stopped with Abort (or its registry TTL).
func StartQRLogin(ctx context.Context, cfg *Config) (*QRFlow, error) {
	if cfg.APIID == 0 || cfg.APIHash == "" {
		return nil, fmt.Errorf("tgclient: API ID and hash are required for QR login")
	}
	storage := &session.StorageMemory{}
	// A dedicated dispatcher is required: qrlogin waits for the
	// updateLoginToken push that Telegram sends when the QR is scanned.
	dispatcher := tg.NewUpdateDispatcher()
	client := telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: storage,
		UpdateHandler:  dispatcher,
	})

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	f := &QRFlow{
		state:      QRWaiting,
		passwordCh: make(chan string, 1),
		cancel:     cancel,
		done:       make(chan struct{}),
	}
	// G118: the flow deliberately detaches from the request context — it is
	// owned by the login registry (TTL/Abort), not by the /authorize request
	// that spawned it.
	go f.run(runCtx, client, dispatcher, storage) //nolint:gosec // G118: see above
	return f, nil
}

func (f *QRFlow) run(ctx context.Context, client *telegram.Client, dispatcher tg.UpdateDispatcher, storage *session.StorageMemory) {
	defer close(f.done)
	defer f.cancel()

	var user QRUser
	runErr := client.Run(ctx, func(ctx context.Context) error {
		loggedIn := qrlogin.OnLoginToken(dispatcher)
		authorization, err := client.QR().Auth(ctx, loggedIn, func(_ context.Context, token qrlogin.Token) error {
			f.setToken(token.URL())
			return nil
		})
		if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
			authorization, err = f.passwordLoop(ctx, client)
		}
		if err != nil {
			return err
		}
		u, ok := authorization.User.AsNotEmpty()
		if !ok {
			return fmt.Errorf("tgclient: QR login returned unexpected user type %T", authorization.User)
		}
		user = QRUser{ID: tgid.UserID(u.ID), Username: u.Username}
		// Return nil to exit Run cleanly: gotd persists the final session
		// state on the way out, and the connection is torn down before we
		// expose the session bytes below.
		return nil
	})
	if runErr != nil {
		f.fail(fmt.Errorf("tgclient: QR login: %w", runErr))
		return
	}

	data, err := storage.LoadSession(context.Background())
	if err != nil {
		f.fail(fmt.Errorf("tgclient: reading QR login session: %w", err))
		return
	}
	f.complete(user, data)
}

// passwordLoop services the 2FA branch: surface PasswordNeeded, then try
// submitted passwords until one works, the attempt budget is exhausted, or
// the flow is canceled.
func (f *QRFlow) passwordLoop(ctx context.Context, client *telegram.Client) (*tg.AuthAuthorization, error) {
	for range qrPasswordAttempts {
		f.setState(QRPasswordNeeded)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case pw := <-f.passwordCh:
			authorization, err := client.Auth().Password(ctx, pw)
			if err == nil {
				return authorization, nil
			}
			if !tgerr.Is(err, "PASSWORD_HASH_INVALID") {
				return nil, fmt.Errorf("2FA password check: %w", err)
			}
			f.recordPasswordError(err)
		}
	}
	return nil, errors.New("too many wrong 2FA password attempts")
}

// TokenURL returns the latest tg://login URL. It rotates when Telegram
// expires a token (roughly every 30 seconds); ok is false until the first
// export completes.
func (f *QRFlow) TokenURL() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenURL, f.tokenURL != ""
}

func (f *QRFlow) State() QRState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

// User is valid once State is QRDone.
func (f *QRFlow) User() (QRUser, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.user, f.state == QRDone
}

// SessionData returns the raw gotd session bytes; valid once State is QRDone.
func (f *QRFlow) SessionData() ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionData, f.state == QRDone
}

// SubmitPassword hands a 2FA password to the flow. Accepted only in
// QRPasswordNeeded; returns false otherwise (including when a previous
// submission is still being verified).
func (f *QRFlow) SubmitPassword(pw string) bool {
	if f.State() != QRPasswordNeeded {
		return false
	}
	select {
	case f.passwordCh <- pw:
		return true
	default:
		return false
	}
}

// Err reports the terminal failure (QRFailed), or the most recent rejected
// 2FA attempt while the state is still QRPasswordNeeded.
func (f *QRFlow) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// Done is closed when the flow has fully finished: the bootstrap client has
// disconnected and either SessionData is final or Err is set.
func (f *QRFlow) Done() <-chan struct{} { return f.done }

// Abort cancels the flow. Idempotent and non-blocking; the flow transitions
// to QRFailed once teardown completes.
func (f *QRFlow) Abort() { f.cancel() }

func (f *QRFlow) setToken(url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenURL = url
}

func (f *QRFlow) setState(s QRState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
}

func (f *QRFlow) recordPasswordError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *QRFlow) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = QRFailed
	f.err = err
}

func (f *QRFlow) complete(user QRUser, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = QRDone
	f.user = user
	f.sessionData = data
	f.err = nil
}
