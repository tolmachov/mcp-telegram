package authsrv

import (
	"context"
	"time"
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
// canceled; Close waits on sweepDone.
func (a *AuthServer) sessionSweeper(ctx context.Context) {
	defer close(a.sweepDone)
	t := time.NewTimer(sweepInitialDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.sweepOrphanSessions(ctx)
			t.Reset(sweepInterval)
		}
	}
}

// sweepOrphanSessions deletes every stored session whose blob is older than
// the refresh-token TTL (plus margin). Safety invariant: login,
// upgrade-on-refresh, and every gotd re-store all WRITE the blob, so its
// mtime >= LoginAt of every token bound to it; the refresh TTL is absolute
// from LoginAt, so age > TTL means every refresh token for the session has
// expired and the session can never be used again. Active sessions are
// re-written by gotd during normal operation and by fresh logins, which keeps
// them out of the cutoff. Individual failures are logged and skipped — the
// next sweep retries.
func (a *AuthServer) sweepOrphanSessions(ctx context.Context) {
	refs, err := a.store.List(ctx)
	if err != nil {
		a.logger.Error("orphan-session sweep: listing sessions failed", "err", err)
		return
	}
	cutoff := a.cfg.refreshTokenTTL() + sweepMargin
	now := a.now()
	for _, ref := range refs {
		age := now.Sub(ref.UpdatedAt)
		if age <= cutoff {
			continue
		}
		if err := a.store.Delete(ctx, ref.UserID, ref.SID); err != nil {
			a.logger.Error("orphan-session sweep: delete failed",
				"user_id", ref.UserID, "session", ref.SID, "err", err)
			continue
		}
		a.logger.Info("orphan-session sweep: deleted unreachable session",
			"user_id", ref.UserID, "session", ref.SID, "age", age.Round(time.Hour))
	}
}
