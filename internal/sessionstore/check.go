package sessionstore

import (
	"context"
	"fmt"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// SessionState describes a stored session, not whether Telegram accepts its key.
// Its zero value is missing, which cannot permit a refresh.
type SessionState int

const (
	SessionMissing SessionState = iota
	SessionRevoked
	SessionPresent
)

// CheckSession observes revocation and existence together, judging revocation
// first. Storage errors are not evidence that the session is missing or revoked.
// The observations are not an atomic snapshot and do not rotate a grant.
func CheckSession(ctx context.Context, store Store, userID tgid.UserID, sid string) (SessionState, error) {
	type observation struct {
		ok  bool
		err error
	}
	existsDone := make(chan observation, 1)
	go func() {
		ok, err := store.Exists(ctx, userID, sid)
		existsDone <- observation{ok, err}
	}()
	revoked, err := store.Revoked(ctx, userID, sid)
	exists := <-existsDone
	switch {
	case err != nil:
		return SessionMissing, fmt.Errorf("checking session revocation: %w", err)
	case revoked:
		return SessionRevoked, nil
	case exists.err != nil:
		return SessionMissing, fmt.Errorf("checking session existence: %w", exists.err)
	case !exists.ok:
		return SessionMissing, nil
	default:
		return SessionPresent, nil
	}
}
