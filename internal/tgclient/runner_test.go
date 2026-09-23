package tgclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gotd/log"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/gotd/td/tgtest"
	"github.com/gotd/td/tgtest/cluster"
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
		tgerr.New(401, "USER_DEACTIVATED_BAN"),
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
	for _, original := range []error{
		ErrSessionUnauthorized,
		tgerr.New(401, "AUTH_KEY_UNREGISTERED"),
		tgerr.New(406, "SESSION_EXPIRED"),
		// Any 401 of the home DC is the verdict, even one another DC may
		// answer while the session stands.
		tgerr.New(401, "SESSION_PASSWORD_NEEDED"),
		tgerr.New(401, "AUTH_KEY_PERM_EMPTY"),
		tgerr.New(500, "INTERNAL_SERVER_ERROR"),
		errors.New("storage unavailable"),
		context.Canceled,
	} {
		r := &Running{cancel: func() {}}
		r.stop(sessionError(original))
		require.ErrorIs(t, r.Err(), original)
		assert.Equal(t, tgerr.IsCode(original, 401) || isSessionRefusal(original) || errors.Is(original, ErrSessionUnauthorized),
			errors.Is(r.Err(), ErrSessionUnauthorized), "%v", original)
	}
}

// newWatchedRunning returns a serving Running as StartClient builds it, the
// context its Run loop would run on, and what it logs.
func newWatchedRunning(t *testing.T) (*Running, context.Context, *bytes.Buffer) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	return &Running{lifetime: ctx, cancel: cancel, logger: logger, now: time.Now}, ctx, &logs
}

// callWatched makes one download-style call through r's refusal watch over inv.
func callWatched(ctx context.Context, r *Running, inv tg.Invoker) error {
	return refusalWatch(r.confirmRefusal).Handle(inv).Invoke(ctx, &tg.UploadGetFileRequest{}, &tg.UploadFileBox{}) //nolint:wrapcheck // the test inspects the watch's reply as is.
}

// refuseFile answers the call with a refusal, as a secondary DC does.
func refuseFile(refusal error) telegramfake.InvokeFunc {
	return telegramfake.Typed(func(context.Context, *tg.UploadGetFileRequest, *tg.UploadFileBox) error { return refusal })
}

// homeCheck answers the home-DC check with check, after verifying it asks
// for the session's own user.
func homeCheck(check func(ctx context.Context) error) telegramfake.InvokeFunc {
	return telegramfake.Typed(func(ctx context.Context, req *tg.UsersGetUsersRequest, out *tg.UserClassVector) error {
		if len(req.ID) != 1 {
			return fmt.Errorf("home check asked for %d users", len(req.ID))
		}
		if _, ok := req.ID[0].(*tg.InputUserSelf); !ok {
			return fmt.Errorf("home check asked for %T, not the session's own user", req.ID[0])
		}
		if err := check(ctx); err != nil {
			return err
		}
		out.Elems = []tg.UserClass{&tg.User{ID: 7}}
		return nil
	})
}

// homeSelf answers the home-DC check with err, or the account when err is nil.
func homeSelf(err error) telegramfake.InvokeFunc {
	return homeCheck(func(context.Context) error { return err })
}

// TestRefusalWatchPassesOtherFailures pins that a failure which is no refusal
// neither asks the home DC nor stops the client.
func TestRefusalWatchPassesOtherFailures(t *testing.T) {
	for _, alive := range []error{errors.New("dial tcp: no route to host"), tgerr.New(420, "FLOOD_WAIT_30"), tgerr.New(400, "CHANNEL_INVALID"), tgerr.New(401, "SESSION_PASSWORD_NEEDED")} {
		r, ctx, _ := newWatchedRunning(t)
		inv := telegramfake.New(refuseFile(alive))
		require.ErrorIs(t, callWatched(t.Context(), r, inv), alive)
		require.NoError(t, r.Err(), "%v must not stop the client", alive)
		require.NoError(t, ctx.Err())
		assert.Len(t, inv.RequestTypes(), 1, "%v must not trigger a home-DC check", alive)
	}
}

// TestRefusalWatchKeepsAHealthySession pins the secondary-DC case: a download
// refused by a DC that never took the exported authorisation, while the home
// DC still accepts the session — or cannot be asked — leaves the client
// serving, and the caller learns which of the two it was.
func TestRefusalWatchKeepsAHealthySession(t *testing.T) {
	unreachable := errors.New("dial tcp: i/o timeout")
	for name, home := range map[string]error{
		"home DC accepts the session": nil,
		"home DC unreachable":         unreachable,
	} {
		t.Run(name, func(t *testing.T) {
			r, ctx, _ := newWatchedRunning(t)
			refusal := tgerr.New(401, "AUTH_KEY_UNREGISTERED")
			inv := telegramfake.New(refuseFile(refusal), homeSelf(home))
			err := callWatched(t.Context(), r, inv)
			require.ErrorIs(t, err, refusal)
			var unconfirmed *UnconfirmedRefusalError
			require.ErrorAs(t, err, &unconfirmed)
			if home == nil {
				require.NoError(t, unconfirmed.Check)
			} else {
				require.ErrorIs(t, unconfirmed.Check, unreachable)
			}
			assert.NotErrorIs(t, err, ErrSessionUnauthorized, "an unconfirmed refusal is no verdict")
			assert.False(t, IsSystemic(err))
			assert.False(t, IsPeerSpecific(err))
			require.NoError(t, r.Err())
			require.NoError(t, ctx.Err())
			assert.Zero(t, inv.Remaining())
		})
	}
}

// TestRefusalWatchReusesTheHomeVerdict pins that the home DC accepting the
// session answers later refusals for homeAcceptedTTL without asking it again,
// and that a failed check is not remembered.
func TestRefusalWatchReusesTheHomeVerdict(t *testing.T) {
	r, _, _ := newWatchedRunning(t)
	now := time.Unix(0, 0)
	r.now = func() time.Time { return now }
	refusal := tgerr.New(401, "AUTH_KEY_UNREGISTERED")
	inv := telegramfake.New(
		refuseFile(refusal), homeSelf(errors.New("dial tcp: i/o timeout")),
		refuseFile(refusal), homeSelf(nil),
		refuseFile(refusal),
		refuseFile(refusal),
		refuseFile(refusal), homeSelf(nil),
	)
	var unconfirmed *UnconfirmedRefusalError

	require.ErrorAs(t, callWatched(t.Context(), r, inv), &unconfirmed)
	require.Error(t, unconfirmed.Check, "the home DC could not be asked")
	require.ErrorAs(t, callWatched(t.Context(), r, inv), &unconfirmed)
	require.NoError(t, unconfirmed.Check, "a failed check is asked again")
	for range 2 {
		now = now.Add(homeAcceptedTTL / 3)
		require.ErrorAs(t, callWatched(t.Context(), r, inv), &unconfirmed)
		require.NoError(t, unconfirmed.Check, "the recent verdict answers without a check")
	}
	now = now.Add(homeAcceptedTTL / 3)
	require.ErrorAs(t, callWatched(t.Context(), r, inv), &unconfirmed)
	require.NoError(t, unconfirmed.Check)
	assert.Zero(t, inv.Remaining(), "the home DC is asked again once the verdict expired")
}

// TestRefusalWatchStopsOnAConfirmedRefusal pins that a refusal the home DC
// confirms stops the client before the call returns, and reaches the caller
// as the verdict.
func TestRefusalWatchStopsOnAConfirmedRefusal(t *testing.T) {
	for _, home := range []error{
		tgerr.New(401, "SESSION_REVOKED"),
		tgerr.New(401, "USER_DEACTIVATED_BAN"),
		// Any 401 of the home DC is the verdict.
		tgerr.New(401, "SESSION_PASSWORD_NEEDED"),
	} {
		r, ctx, _ := newWatchedRunning(t)
		refusal := tgerr.New(401, "AUTH_KEY_UNREGISTERED")
		inv := telegramfake.New(refuseFile(refusal), homeSelf(home))
		err := callWatched(t.Context(), r, inv)
		require.ErrorIs(t, err, refusal, "the caller still sees Telegram's own error")
		require.ErrorIs(t, err, ErrSessionUnauthorized)
		assert.True(t, IsSystemic(err))
		require.ErrorIs(t, r.Err(), ErrSessionUnauthorized)
		require.ErrorIs(t, r.Err(), home, "the client keeps the home DC's reply")
		require.Error(t, ctx.Err(), "the Run loop is told to stop")

		// A later reason never overwrites the refusal.
		r.stop(errClosed)
		require.ErrorIs(t, r.Err(), ErrSessionUnauthorized)
	}
}

// TestRefusalWatchAfterTheClientStopped pins that a call refused once the
// client has stopped asks nothing more of the home DC: after a confirmed
// refusal it gets the verdict, after any other stop why the client stopped.
func TestRefusalWatchAfterTheClientStopped(t *testing.T) {
	refusal := tgerr.New(401, "AUTH_KEY_UNREGISTERED")

	r, _, _ := newWatchedRunning(t)
	r.stop(sessionError(tgerr.New(401, "SESSION_REVOKED")))
	inv := telegramfake.New(refuseFile(refusal))
	err := callWatched(t.Context(), r, inv)
	require.ErrorIs(t, err, ErrSessionUnauthorized)
	require.ErrorIs(t, err, refusal)
	assert.Zero(t, inv.Remaining())

	r, _, _ = newWatchedRunning(t)
	r.stop(errClosed)
	inv = telegramfake.New(refuseFile(refusal))
	err = callWatched(t.Context(), r, inv)
	var unconfirmed *UnconfirmedRefusalError
	require.ErrorAs(t, err, &unconfirmed)
	require.ErrorIs(t, unconfirmed.Check, errClosed)
	assert.NotErrorIs(t, err, ErrSessionUnauthorized)
	assert.Zero(t, inv.Remaining())
}

// TestRefusalCheckEndsWithTheClient pins the check's own context: it has a
// deadline, and a client stopping while the check runs answers the refused
// call with why it stopped, without a warning about an unreachable home DC.
func TestRefusalCheckEndsWithTheClient(t *testing.T) {
	r, _, logs := newWatchedRunning(t)
	refusal := tgerr.New(401, "AUTH_KEY_UNREGISTERED")
	inv := telegramfake.New(refuseFile(refusal), homeCheck(func(ctx context.Context) error {
		_, bounded := ctx.Deadline()
		assert.True(t, bounded, "the check must be bounded")
		r.stop(errClosed)
		<-ctx.Done()
		return ctx.Err()
	}))
	err := callWatched(t.Context(), r, inv)
	require.ErrorIs(t, err, refusal)
	var unconfirmed *UnconfirmedRefusalError
	require.ErrorAs(t, err, &unconfirmed)
	require.ErrorIs(t, unconfirmed.Check, errClosed)
	require.ErrorIs(t, r.Err(), errClosed)
	assert.Empty(t, logs.String(), "a client stopping is no reason to warn")
}

// TestRefusalWatchSharesOneDetachedCheck pins that calls refused while a
// home-DC check runs wait on that check instead of starting their own, and
// that the check belongs to none of them: the caller that started it giving
// up neither cancels it nor fails the others.
func TestRefusalWatchSharesOneDetachedCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, _, _ := newWatchedRunning(t)
		refusal := tgerr.New(401, "AUTH_KEY_UNREGISTERED")
		release := make(chan struct{})
		const callers = 4
		// The first refused call, then its home-DC check, then the calls
		// refused while the check runs.
		script := []telegramfake.InvokeFunc{
			refuseFile(refusal),
			homeCheck(func(ctx context.Context) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-release:
					return tgerr.New(401, "AUTH_KEY_UNREGISTERED")
				}
			}),
		}
		for range callers - 1 {
			script = append(script, refuseFile(refusal))
		}
		inv := telegramfake.New(script...)

		// The first caller starts the check and gives up waiting on it.
		gone, leave := context.WithCancel(t.Context())
		first := make(chan error, 1)
		go func() { first <- callWatched(gone, r, inv) }()
		synctest.Wait()
		leave()
		err := <-first
		require.ErrorIs(t, err, refusal)
		require.ErrorIs(t, err, context.Canceled, "a caller that left gets its own reason too")
		assert.True(t, IsSystemic(err), "so a batch stops on it")
		assert.NotErrorIs(t, err, ErrSessionUnauthorized)

		errs := make(chan error, callers-1)
		for range callers - 1 {
			go func() { errs <- callWatched(t.Context(), r, inv) }()
		}
		synctest.Wait() // every caller now waits on the one check
		require.NoError(t, r.Err(), "the check outlived the caller that started it")
		close(release)
		for range callers - 1 {
			require.ErrorIs(t, <-errs, ErrSessionUnauthorized)
		}
		require.ErrorIs(t, r.Err(), ErrSessionUnauthorized)
		assert.Zero(t, inv.Remaining(), "one home-DC check served every refused call")
	})
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

// startOnCluster starts a client through startClient against an in-process
// Telegram cluster whose home DC answers users.getUsers with getUsers and
// upload.getFile with getFile, the n-th call of each getting its n-th answer.
func startOnCluster(t *testing.T, onFloodWait FloodWaitCallback, getUsers, getFile []func(*tgtest.Server, *tgtest.Request) error) (*Running, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)

	c := cluster.NewCluster(cluster.Options{})
	scripted := func(answers []func(*tgtest.Server, *tgtest.Request) error) func(*tgtest.Server, *tgtest.Request) error {
		var mu sync.Mutex
		return func(server *tgtest.Server, req *tgtest.Request) error {
			mu.Lock()
			if len(answers) == 0 {
				mu.Unlock()
				return server.SendErr(req, tgerr.New(500, "UNSCRIPTED_CALL"))
			}
			answer := answers[0]
			answers = answers[1:]
			mu.Unlock()
			return answer(server, req)
		}
	}
	// A client with an empty session starts on DC 2.
	c.Dispatch(2, "home").
		HandleFunc(tg.UsersGetUsersRequestTypeID, scripted(getUsers)).
		HandleFunc(tg.UploadGetFileRequestTypeID, scripted(getFile))
	up := make(chan error, 1)
	go func() { up <- c.Up(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-up
	})
	select {
	case <-c.Ready():
	case err := <-up:
		t.Fatalf("cluster did not come up: %v", err)
	}

	r, err := startClient(ctx, &Config{APIID: 1, APIHash: "hash"}, &session.StorageMemory{}, discardLogger(), onFloodWait, telegram.Options{
		PublicKeys: c.Keys(),
		Resolver:   c.Resolver(),
		DCList:     c.List(),
	})
	if r != nil {
		t.Cleanup(r.Close)
	}
	return r, err //nolint:wrapcheck // the test inspects startClient's answer as is.
}

func answerErr(err *tgerr.Error) func(*tgtest.Server, *tgtest.Request) error {
	return func(server *tgtest.Server, req *tgtest.Request) error { return server.SendErr(req, err) }
}

func answerSelf(server *tgtest.Server, req *tgtest.Request) error {
	return server.SendVector(req, &tg.User{ID: 7, AccessHash: 70}) //nolint:wrapcheck // the fake server hands the send error to tgtest as is.
}

// TestStartClientWatchesRefusalsInsideTheFloodWaiter pins the real client's
// wiring: a call goes through the flood waiter, and the refusal the retried
// call gets goes through refusalWatch, which confirms it on the home DC and
// stops the client.
func TestStartClientWatchesRefusalsInsideTheFloodWaiter(t *testing.T) {
	t.Parallel()
	var waits atomic.Int32
	r, err := startOnCluster(t, func(context.Context, time.Duration) { waits.Add(1) },
		[]func(*tgtest.Server, *tgtest.Request) error{answerSelf, answerErr(tgerr.New(401, "SESSION_REVOKED"))},
		[]func(*tgtest.Server, *tgtest.Request) error{answerErr(tgerr.New(420, "FLOOD_WAIT_0")), answerErr(tgerr.New(401, "AUTH_KEY_UNREGISTERED"))},
	)
	require.NoError(t, err)
	assert.Equal(t, int64(7), r.Self().ID)

	_, err = r.API().UploadGetFile(t.Context(), &tg.UploadGetFileRequest{Location: &tg.InputDocumentFileLocation{}, Limit: 1024})
	require.ErrorIs(t, err, ErrSessionUnauthorized, "the refusal was confirmed on the home DC")
	assert.Equal(t, int32(1), waits.Load(), "the flood waiter retried the call first")
	<-r.Done()
	require.ErrorIs(t, r.Err(), ErrSessionUnauthorized)
}

// TestStartClientKeepsTheHomeDCsRefusal pins that a readiness check the home
// DC refuses — with any 401, not only the codes another DC's refusal is
// judged by — is the verdict and keeps Telegram's own code.
func TestStartClientKeepsTheHomeDCsRefusal(t *testing.T) {
	t.Parallel()
	_, err := startOnCluster(t, nil,
		[]func(*tgtest.Server, *tgtest.Request) error{answerErr(tgerr.New(401, "SESSION_PASSWORD_NEEDED"))},
		nil,
	)
	require.ErrorIs(t, err, ErrSessionUnauthorized)
	assert.True(t, tgerr.Is(err, "SESSION_PASSWORD_NEEDED"), "the verdict keeps the home DC's code: %v", err)
}
