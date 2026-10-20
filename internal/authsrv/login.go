package authsrv

import (
	"context"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// LoginState is the observable phase of one in-flight Telegram QR login.
type LoginState int

const (
	// LoginWaiting means the QR code is displayed and not yet scanned (or
	// scanned but not yet confirmed on the device).
	LoginWaiting LoginState = iota
	// LoginPasswordNeeded means the account has two-factor auth enabled and
	// the cloud password must be submitted to finish the login.
	LoginPasswordNeeded
	// LoginPasswordChecking means a submitted 2FA password is being verified
	// with Telegram; further submissions are ignored until verification ends
	// in LoginPasswordNeeded (wrong password), LoginDone or LoginFailed.
	LoginPasswordChecking
	// LoginDone means the bootstrap client stopped and Result is ready.
	LoginDone
	// LoginFailed means the login failed terminally: the snapshot reports why.
	LoginFailed
)

// LoginUser identifies the Telegram account that completed a QR login.
type LoginUser struct {
	ID       tgid.UserID
	Username string
}

// LoginSnapshot is a coherent observation of one pending Telegram login.
// Session credentials are deliberately available only through Result.
type LoginSnapshot struct {
	State        LoginState
	TokenURL     string
	QRRevision   int
	Rejected     int
	AttemptsLeft int
	Err          error
}

// LoginResult is a completed Telegram identity and its final session bytes.
type LoginResult struct {
	User        LoginUser
	SessionData []byte
}

// LoginFlow is one in-flight Telegram QR login. Observing LoginDone guarantees
// Result is ready: the bootstrap client has stopped and the session is final.
// Terminal states do not change, so callers need no separate teardown wait.
type LoginFlow interface {
	Observe() LoginSnapshot
	// Result never blocks and returns a defensive session copy on success.
	Result() (LoginResult, bool)
	// SubmitPassword accepts one password only while awaiting one. Verification
	// does not spend an attempt; only a genuine rejection does.
	SubmitPassword(pw string) bool
	// Abort cancels the flow; idempotent and non-blocking.
	Abort()
}

// StartLoginFunc launches a fresh Telegram QR login. The context carries
// values and gates only startup; the returned flow detaches and runs until it
// completes, its registry TTL expires, or Abort is called — cancelling ctx
// afterwards does NOT stop it.
type StartLoginFunc func(ctx context.Context) (LoginFlow, error)
