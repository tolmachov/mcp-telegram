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
	QRWaiting           QRState = iota // QR shown, waiting for a scan
	QRPasswordNeeded                   // account has 2FA; waiting for the password
	QRPasswordVerifying                // a submitted password is being checked, or the check ended and the flow is about to reopen the prompt or fail
	QRDone                             // logged in, session bytes final
	QRFailed                           // terminal failure (incl. abort/expiry)
)

// QRUser identifies the account that accepted the QR login.
type QRUser struct {
	ID       tgid.UserID
	Username string
}

// QRSnapshot is one coherent observation of a QR login. It contains no
// session credentials; Result returns them only after successful teardown.
type QRSnapshot struct {
	State        QRState
	TokenURL     string
	QRRevision   int
	Rejected     int
	AttemptsLeft int
	Err          error
}

// QRResult is a completed login's identity and final session credentials.
type QRResult struct {
	User        QRUser
	SessionData []byte
}

// qrPasswordAttempts is the number of 2FA passwords a login may try before
// it fails.
const qrPasswordAttempts = 3

// QRFlow is one Telegram QR login running on a background goroutine with its
// own short-lived client and an in-memory session. The session deliberately
// never touches disk here: the caller decides where the final bytes go
// (Result) only after checking who actually scanned the code.
//
// State transitions: Waiting → (PasswordNeeded ⇄ PasswordVerifying →)
// Done | Failed. Success is published only after client.Run returns and
// the final session bytes are loaded, so observing Done guarantees Result
// is ready without a separate wait. No bootstrap connection remains when
// callers hand the session to a long-lived client.
type QRFlow struct {
	mu          sync.Mutex
	tokenURL    string
	qrRevision  int
	state       QRState
	user        QRUser
	sessionData []byte
	err         error
	rejected    int // 2FA passwords Telegram has rejected

	passwordCh chan string
	cancel     context.CancelFunc
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
	}
	// G118: the flow deliberately detaches from the request context — it is
	// owned by the login registry (TTL/Abort), not by the /authorize request
	// that spawned it.
	//nolint:gosec // G118: the registry owns this detached flow until completion or Abort.
	go f.run(runCtx, client.Run, func(ctx context.Context) (QRUser, error) {
		return f.authenticate(ctx, client, dispatcher)
	}, storage)
	return f, nil
}

// run owns publication: neither authentication nor intermediate session
// writes make a result ready before runClient finishes disconnecting.
func (f *QRFlow) run(ctx context.Context, runClient func(context.Context, func(context.Context) error) error, authenticate func(context.Context) (QRUser, error), storage session.Storage) {
	defer f.cancel()
	var user QRUser
	runErr := runClient(ctx, func(ctx context.Context) error {
		var err error
		user, err = authenticate(ctx)
		return err
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

// authenticate runs inside client.Run. Returning a user lets gotd persist
// its final session and disconnect before run publishes success.
func (f *QRFlow) authenticate(ctx context.Context, client *telegram.Client, dispatcher tg.UpdateDispatcher) (QRUser, error) {
	loggedIn := qrlogin.OnLoginToken(dispatcher)
	authorization, err := client.QR().Auth(ctx, loggedIn, func(_ context.Context, token qrlogin.Token) error {
		f.setToken(token.URL())
		return nil
	})
	if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
		authorization, err = f.passwordLoop(ctx, client.Auth().Password)
	}
	if err != nil {
		return QRUser{}, fmt.Errorf("authenticating Telegram QR login: %w", err)
	}
	u, ok := authorization.User.AsNotEmpty()
	if !ok {
		return QRUser{}, fmt.Errorf("tgclient: QR login returned unexpected user type %T", authorization.User)
	}
	user := QRUser{ID: tgid.UserID(u.ID), Username: u.Username}
	return user, nil
}

// passwordLoop services the 2FA branch: surface PasswordNeeded, then try
// submitted passwords with check until one works, the attempt budget is
// exhausted, or the flow is cancelled. Only the loop reopens PasswordNeeded,
// and only while an attempt remains: once the loop gives up the state stays
// PasswordVerifying until the flow fails, so no password is accepted that
// nothing would read.
func (f *QRFlow) passwordLoop(ctx context.Context, check func(context.Context, string) (*tg.AuthAuthorization, error)) (*tg.AuthAuthorization, error) {
	for range qrPasswordAttempts {
		f.setState(QRPasswordNeeded)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case pw := <-f.passwordCh:
			authorization, err := check(ctx, pw)
			if err == nil {
				return authorization, nil
			}
			if !tgerr.Is(err, "PASSWORD_HASH_INVALID") {
				return nil, fmt.Errorf("2FA password check: %w", err)
			}
			f.rejectPassword()
		}
	}
	return nil, errors.New("too many wrong 2FA password attempts")
}

// Observe returns the phase, QR token and password counters under one lock.
// A success is terminal: Result stays ready even if Abort is called later.
func (f *QRFlow) Observe() QRSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return QRSnapshot{
		State: f.state, TokenURL: f.tokenURL, QRRevision: f.qrRevision,
		Rejected: f.rejected, AttemptsLeft: qrPasswordAttempts - f.rejected, Err: f.err,
	}
}

// Result never blocks. It is ready whenever Observe reports QRDone and
// returns its own copy of the session so a caller cannot change it.
func (f *QRFlow) Result() (QRResult, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != QRDone {
		return QRResult{}, false
	}
	return QRResult{User: f.user, SessionData: append([]byte(nil), f.sessionData...)}, true
}

// SubmitPassword hands a 2FA password to the flow. Accepted only in
// QRPasswordNeeded; returns false otherwise (including when a previous
// submission is still being verified). The send never blocks: passwordLoop
// opens QRPasswordNeeded only with passwordCh drained, and leaving that
// state here admits a single password per opening.
func (f *QRFlow) SubmitPassword(pw string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != QRPasswordNeeded {
		return false
	}
	f.state = QRPasswordVerifying
	f.passwordCh <- pw
	return true
}

// Abort cancels the flow. Idempotent and non-blocking; the flow transitions
// to QRFailed once teardown completes.
func (f *QRFlow) Abort() { f.cancel() }

func (f *QRFlow) setToken(url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if url != f.tokenURL {
		f.tokenURL = url
		f.qrRevision++
	}
}

func (f *QRFlow) setState(s QRState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
}

func (f *QRFlow) rejectPassword() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected++
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
	f.sessionData = append([]byte(nil), data...)
}
