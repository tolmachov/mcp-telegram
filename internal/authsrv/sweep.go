package authsrv

import (
	"context"
	"errors"
	"runtime/debug"
	"time"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// Orphan-session sweep cadence. A client that drops its tokens without
// revoking leaves its session object behind; the sweeper reclaims those.
const (
	// sweepInitialDelay keeps the first sweep off the startup path (Cloud Run
	// cold starts) while still running long before the first interval tick.
	sweepInitialDelay = time.Minute
	sweepInterval     = 6 * time.Hour
	// sweepMargin pads the refresh-token TTL against clock skew between the
	// storage backend's mtime and this process's clock.
	sweepMargin = 24 * time.Hour
)

// sessionSweeper periodically deletes unreachable session blobs. Same
// lifecycle as the pending-login janitor: runs until ctx (loginCtx) is
// cancelled; Close waits on sweepDone.
func (a *AuthServer) sessionSweeper(ctx context.Context) {
	defer close(a.sweepDone)
	t := time.NewTimer(sweepInitialDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.runSweep(ctx)
			t.Reset(sweepInterval)
		}
	}
}

// runIsolated runs one maintenance task (a login abort, one part of a sweep),
// isolating everything else from its panic — a store backend bug, a
// misbehaving LoginFlow.Abort. The panic is logged with its stack and the
// other tasks, and later ticks, still run.
func (a *AuthServer) runIsolated(task string, run func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			a.logger.Error("auth background task panicked; recovered",
				"task", task, "panic", recovered, "stack", string(debug.Stack()))
		}
	}()
	run()
}

// runSweep executes one storage sweep iteration.
//
// Session blobs: login and every gotd re-store both WRITE the blob, so its
// mtime >= LoginAt of every token bound to it; the refresh TTL is absolute
// from LoginAt, so age > TTL means every refresh token for the session has
// expired and the session can never be used again. Active sessions are
// re-written by gotd during normal operation and by fresh logins, which keeps
// them out of the cutoff.
//
// Revocation tombstones: a tombstone's mtime is its revoke time, and a
// session's LoginAt precedes its revoke time, so once the tombstone is older
// than the cutoff no refresh token for that session can still be valid — the
// tombstone has done its job and can go.
//
// Each part runs isolated, so a part that panics on every sweep cannot keep
// the others from running.
func (a *AuthServer) runSweep(ctx context.Context) {
	a.runIsolated("session sweep", func() { a.sweepRefs(ctx, "sessions", a.store.List, a.store.Delete) })
	a.runIsolated("tombstone sweep", func() { a.sweepRefs(ctx, "tombstones", a.store.ListRevoked, a.store.DeleteRevoked) })
	a.runIsolated("grant sweep", func() {
		undecodable, err := a.store.SweepAuthState(ctx, a.now())
		for _, family := range undecodable {
			a.logger.Warn("oauth state sweep: deleted an undecodable grant record",
				"family", family)
		}
		if err != nil && !cancelledOnly(err) {
			a.logger.Error("oauth state sweep failed", "err", err)
		}
	})
}

// cancelledOnly reports whether err consists of cancellation alone: every
// leaf of its wrap and join tree is context.Canceled. Shutdown cancels a
// sweep's context, and the failures that causes are not worth logging; a real
// failure joined with them still is.
func cancelledOnly(err error) bool {
	switch e := err.(type) { //nolint:errorlint // walks the wrap tree itself, leaf by leaf
	case interface{ Unwrap() []error }:
		for _, inner := range e.Unwrap() {
			if !cancelledOnly(inner) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return cancelledOnly(e.Unwrap())
	default:
		return errors.Is(err, context.Canceled)
	}
}

// sweepRefs deletes every entry list returns whose UpdatedAt is older than
// the refresh-token TTL plus sweepMargin. Individual failures are logged and
// skipped — the next sweep retries. Shutdown (ctx ending) stops it without
// logging the failures that cancellation causes.
func (a *AuthServer) sweepRefs(
	ctx context.Context,
	label string,
	list func(context.Context) ([]sessionstore.SessionRef, error),
	del func(context.Context, tgid.UserID, string) error,
) {
	refs, err := list(ctx)
	if err != nil {
		if !cancelledOnly(err) {
			a.logger.Error("auth sweep: listing failed", "sweep", label, "err", err)
		}
		return
	}
	cutoff := refreshTokenTTL + sweepMargin
	now := a.now()
	for _, ref := range refs {
		if ctx.Err() != nil {
			return
		}
		age := now.Sub(ref.UpdatedAt)
		if age <= cutoff {
			continue
		}
		if err := del(ctx, ref.UserID, ref.SID); err != nil {
			if !cancelledOnly(err) {
				a.logger.Error("auth sweep: delete failed",
					"sweep", label, "user_id", ref.UserID, "session", ref.SID, "err", err)
			}
			continue
		}
		a.logger.Info("auth sweep: deleted expired entry",
			"sweep", label, "user_id", ref.UserID, "session", ref.SID, "age", age.Round(time.Hour))
	}
}
