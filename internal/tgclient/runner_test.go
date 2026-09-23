package tgclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/gotd/log"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	telegramfake "github.com/tolmachov/mcp-telegram/internal/testutil/telegram"
)

// TestIsSessionRefusalSeparatesRejectionFromUnreachable guards the distinction
// the re-check depends on. Telegram rejecting the stored key is a verdict
// ("log in again"); anything else is an undetermined check ("retry"). Getting
// this wrong is not cosmetic: a revoked session fails in the connect phase, so
// the callback never runs and there is no Status to read — without this test
// the tool reports check_failed and advises the user to check their network
// for a session they revoked themselves.
func TestIsSessionRefusalSeparatesRejectionFromUnreachable(t *testing.T) {
	dead := []error{
		tgerr.New(401, "AUTH_KEY_UNREGISTERED"),
		tgerr.New(406, "SESSION_EXPIRED"),
		tgerr.New(406, "AUTH_KEY_DUPLICATED"),
		tgerr.New(401, "SESSION_REVOKED"),
		tgerr.New(401, "AUTH_KEY_INVALID"),
		tgerr.New(401, "USER_DEACTIVATED"),
		// Must survive the wrapping gotd applies on the way out.
		fmt.Errorf("callback: %w", tgerr.New(401, "AUTH_KEY_UNREGISTERED")),
	}
	for _, err := range dead {
		assert.True(t, isSessionRefusal(err), "expected a dead-session refusal for %v", err)
	}

	alive := []error{
		errors.New("dial tcp: no route to host"),
		context.DeadlineExceeded,
		tgerr.New(420, "FLOOD_WAIT_30"),
		tgerr.New(500, "INTERNAL_SERVER_ERROR"),
		// 401s that leave the session standing.
		tgerr.New(401, "SESSION_PASSWORD_NEEDED"),
		tgerr.New(401, "AUTH_KEY_PERM_EMPTY"),
		tgerr.New(401, "SOMETHING_NEW"),
	}
	for _, err := range alive {
		assert.False(t, isSessionRefusal(err), "expected an undetermined check for %v", err)
	}
}

func TestRunningPreservesCauseAndClassifiesSessionErrors(t *testing.T) {
	for _, original := range []error{ErrSessionUnauthorized, tgerr.New(401, "AUTH_KEY_UNREGISTERED"), tgerr.New(406, "SESSION_EXPIRED"), errors.New("storage unavailable"), context.Canceled} {
		r := &Running{cancel: func() {}}
		r.stop(sessionError(original))
		require.ErrorIs(t, r.Err(), original)
		assert.Equal(t, isSessionRefusal(original) || errors.Is(original, ErrSessionUnauthorized), errors.Is(r.Err(), ErrSessionUnauthorized))
	}
}

// newWatchedRunning returns a serving Running as StartClient builds it, and
// the context its Run loop would run on.
func newWatchedRunning(t *testing.T) (*Running, context.Context) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return &Running{lifetime: ctx, cancel: cancel, logger: discardLogger()}, ctx
}

// callWatched makes one download-style call through r's refusal watch over inv.
func callWatched(t *testing.T, r *Running, inv tg.Invoker) error {
	return refusalWatch(r.confirmRefusal).Handle(inv).Invoke(t.Context(), &tg.UploadGetFileRequest{}, &tg.UploadFileBox{}) //nolint:wrapcheck // the test inspects the watch's reply as is.
}

// refuseFile answers the call with a refusal, as a secondary DC does.
func refuseFile(refusal error) telegramfake.InvokeFunc {
	return telegramfake.Typed(func(context.Context, *tg.UploadGetFileRequest, *tg.UploadFileBox) error { return refusal })
}

// homeSelf answers the home-DC check with err, or the account when err is nil.
func homeSelf(err error) telegramfake.InvokeFunc {
	return telegramfake.Typed(func(_ context.Context, req *tg.UsersGetUsersRequest, out *tg.UserClassVector) error {
		if len(req.ID) != 1 {
			return fmt.Errorf("home check asked for %d users", len(req.ID))
		}
		if _, ok := req.ID[0].(*tg.InputUserSelf); !ok {
			return fmt.Errorf("home check asked for %T, not the session's own user", req.ID[0])
		}
		if err != nil {
			return err
		}
		out.Elems = []tg.UserClass{&tg.User{ID: 7}}
		return nil
	})
}

// TestRefusalWatchPassesOtherFailures pins that a failure which is no refusal
// neither asks the home DC nor stops the client.
func TestRefusalWatchPassesOtherFailures(t *testing.T) {
	for _, alive := range []error{errors.New("dial tcp: no route to host"), tgerr.New(420, "FLOOD_WAIT_30"), tgerr.New(400, "CHANNEL_INVALID"), tgerr.New(401, "SESSION_PASSWORD_NEEDED")} {
		r, ctx := newWatchedRunning(t)
		inv := telegramfake.New(refuseFile(alive))
		require.ErrorIs(t, callWatched(t, r, inv), alive)
		require.NoError(t, r.Err(), "%v must not stop the client", alive)
		require.NoError(t, ctx.Err())
		assert.Len(t, inv.RequestTypes(), 1, "%v must not trigger a home-DC check", alive)
	}
}

// TestRefusalWatchKeepsAHealthySession pins the secondary-DC case: a download
// refused by a DC that never took the exported authorisation, while the home
// DC still accepts the session — or cannot be asked — leaves the client
// serving, and the caller sees Telegram's own reply, not a verdict.
func TestRefusalWatchKeepsAHealthySession(t *testing.T) {
	for name, home := range map[string]error{
		"home DC accepts the session": nil,
		"home DC unreachable":         errors.New("dial tcp: i/o timeout"),
	} {
		t.Run(name, func(t *testing.T) {
			r, ctx := newWatchedRunning(t)
			refusal := tgerr.New(401, "AUTH_KEY_UNREGISTERED")
			inv := telegramfake.New(refuseFile(refusal), homeSelf(home))
			err := callWatched(t, r, inv)
			require.ErrorIs(t, err, refusal)
			assert.NotErrorIs(t, err, ErrSessionUnauthorized, "an unconfirmed refusal is no verdict")
			assert.False(t, IsSystemic(err))
			require.NoError(t, r.Err())
			require.NoError(t, ctx.Err())
			assert.Zero(t, inv.Remaining())
		})
	}
}

// TestRefusalWatchStopsOnAConfirmedRefusal pins that a refusal the home DC
// confirms stops the client before the call returns, and reaches the caller
// as the verdict.
func TestRefusalWatchStopsOnAConfirmedRefusal(t *testing.T) {
	r, ctx := newWatchedRunning(t)
	refusal := tgerr.New(401, "SESSION_REVOKED")
	inv := telegramfake.New(refuseFile(refusal), homeSelf(tgerr.New(401, "SESSION_REVOKED")))
	err := callWatched(t, r, inv)
	require.ErrorIs(t, err, refusal, "the caller still sees Telegram's own error")
	require.ErrorIs(t, err, ErrSessionUnauthorized)
	assert.True(t, IsSystemic(err))
	require.ErrorIs(t, r.Err(), ErrSessionUnauthorized)
	require.Error(t, ctx.Err(), "the Run loop is told to stop")

	// A later reason never overwrites the refusal.
	r.stop(errClosed)
	require.ErrorIs(t, r.Err(), ErrSessionUnauthorized)
}

// TestRefusalWatchSharesOneCheck pins that calls refused while a home-DC check
// runs wait on that check instead of starting their own, and that a caller
// giving up does not abandon it.
func TestRefusalWatchSharesOneCheck(t *testing.T) {
	r, _ := newWatchedRunning(t)
	refusal := tgerr.New(401, "AUTH_KEY_UNREGISTERED")
	entered, release := make(chan struct{}), make(chan struct{})
	const callers = 4
	// The first refused call, then its home-DC check, then the calls
	// refused while the check runs.
	script := []telegramfake.InvokeFunc{
		refuseFile(refusal),
		telegramfake.Typed(func(_ context.Context, _ *tg.UsersGetUsersRequest, _ *tg.UserClassVector) error {
			close(entered)
			<-release
			return tgerr.New(401, "AUTH_KEY_UNREGISTERED")
		}),
	}
	for range callers - 1 {
		script = append(script, refuseFile(refusal))
	}
	inv := telegramfake.New(script...)

	// The first caller starts the check and gives up waiting on it.
	gone, leave := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() {
		first <- refusalWatch(r.confirmRefusal).Handle(inv).Invoke(gone, &tg.UploadGetFileRequest{}, &tg.UploadFileBox{})
	}()
	<-entered
	leave()
	require.NotErrorIs(t, <-first, ErrSessionUnauthorized, "a caller that left before the verdict gets the bare refusal")

	errs := make(chan error, callers-1)
	for range callers - 1 {
		go func() { errs <- callWatched(t, r, inv) }()
	}
	require.Eventually(t, func() bool { return len(inv.RequestTypes()) == callers+1 }, time.Second, time.Millisecond)
	close(release)
	for range callers - 1 {
		require.ErrorIs(t, <-errs, ErrSessionUnauthorized)
	}
	require.ErrorIs(t, r.Err(), ErrSessionUnauthorized)
	assert.Zero(t, inv.Remaining(), "one home-DC check served every refused call")
}

// TestStartTimeoutErrorCarriesTheConnectionFailure pins that a client which
// never becomes ready reports why: the last connection failure gotd reported
// while it kept retrying, or that none was reported.
func TestStartTimeoutErrorCarriesTheConnectionFailure(t *testing.T) {
	r := &Running{}
	assert.ErrorContains(t, r.startTimeoutError(), "no connection failure was reported")

	dial := errors.New("dial tcp 149.154.167.51:443: i/o timeout")
	r.connDead(errors.New("earlier failure"))
	r.connDead(dial)
	err := r.startTimeoutError()
	require.ErrorIs(t, err, dial)
	assert.ErrorContains(t, err, startClientTimeout.String())
}

// TestGotdLoggerKeepsWarningsOnly pins gotd's log floor: its per-connection
// Info chatter is dropped, its warnings reach the server's handler.
func TestGotdLoggerKeepsWarningsOnly(t *testing.T) {
	var buf bytes.Buffer
	logger := gotdLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	logger.Log(t.Context(), log.LevelInfo, "Starting")
	logger.Log(t.Context(), log.LevelWarn, "Permanent connection error, will not reconnect")
	assert.NotContains(t, buf.String(), "Starting")
	assert.Contains(t, buf.String(), "Permanent connection error")
	assert.Contains(t, buf.String(), "subsystem=gotd")
}

// loadFailingStorage fails LoadSession, which gotd reads in Run before it
// connects — the "callback never runs" route out of StartClient.
type loadFailingStorage struct{ err error }

func (s loadFailingStorage) LoadSession(context.Context) ([]byte, error) { return nil, s.err }
func (s loadFailingStorage) StoreSession(context.Context, []byte) error  { return nil }

// blockingStorage parks LoadSession until the client's own context ends.
type blockingStorage struct{}

func (blockingStorage) LoadSession(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingStorage) StoreSession(context.Context, []byte) error { return nil }

// TestStartClientClassifiesFailuresBeforeTheCallback pins that a session
// Telegram rejects before the ready callback runs still comes back as
// ErrSessionUnauthorized, while any other early failure keeps its cause and
// is not a verdict.
func TestStartClientClassifiesFailuresBeforeTheCallback(t *testing.T) {
	cfg := &Config{APIID: 1, APIHash: "hash"}

	_, err := StartClient(t.Context(), cfg, loadFailingStorage{err: tgerr.New(401, "AUTH_KEY_UNREGISTERED")}, discardLogger(), nil)
	require.ErrorIs(t, err, ErrSessionUnauthorized)

	storageErr := errors.New("keychain access denied")
	_, err = StartClient(t.Context(), cfg, loadFailingStorage{err: storageErr}, discardLogger(), nil)
	require.ErrorIs(t, err, storageErr)
	assert.NotErrorIs(t, err, ErrSessionUnauthorized)
}

// TestStartClientCancelledIsNotAVerdict pins that a startup cut short by its
// caller reports the cancellation, never a dead session.
func TestStartClientCancelledIsNotAVerdict(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := StartClient(ctx, &Config{APIID: 1, APIHash: "hash"}, blockingStorage{}, discardLogger(), nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrSessionUnauthorized)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
