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
	// LoginDone means the login succeeded: User and SessionData are valid.
	LoginDone
	// LoginFailed means the login failed terminally: Err reports why.
	LoginFailed
)

// LoginUser identifies the Telegram account that completed a QR login.
type LoginUser struct {
	ID       tgid.UserID
	Username string
}

// LoginFlow is one in-flight Telegram QR login running on a background
// goroutine, implemented by the tgclient package and consumed here. Done()
// is closed only after the underlying client has fully stopped and the
// session bytes are final, so a receive from Done() in state LoginDone never
// blocks and never races the session data.
type LoginFlow interface {
	// TokenURL returns the latest tg://login URL to encode as a QR code.
	// Telegram rotates it roughly every 30 seconds; ok is false before the
	// first token has been exported.
	TokenURL() (url string, ok bool)
	// State returns the current phase of the flow.
	State() LoginState
	// User returns the logged-in account; valid only in LoginDone.
	User() (LoginUser, bool)
	// SessionData returns the raw gotd session bytes; valid only in LoginDone.
	SessionData() ([]byte, bool)
	// SubmitPassword feeds the 2FA cloud password to the flow. It is
	// accepted only in LoginPasswordNeeded; a wrong password keeps the state
	// at LoginPasswordNeeded (Err then reports the last attempt error).
	SubmitPassword(pw string) bool
	// Err reports the failure in LoginFailed, and the last rejected password
	// attempt while in LoginPasswordNeeded. Nil otherwise.
	Err() error
	// Done is closed when the flow has fully terminated (success or not).
	Done() <-chan struct{}
	// Abort cancels the flow; idempotent and non-blocking.
	Abort()
}

// StartLoginFunc launches a fresh Telegram QR login. The context carries
// values and gates only startup; the returned flow detaches and runs until it
// completes, its registry TTL expires, or Abort is called — canceling ctx
// afterwards does NOT stop it.
type StartLoginFunc func(ctx context.Context) (LoginFlow, error)
