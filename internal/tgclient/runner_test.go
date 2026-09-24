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
	"github.com/gotd/td/bin"
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
// the verdict depends on. Telegram rejecting the stored key is a verdict
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
		// A session that cannot serve for any other 401 is refused too.
		tgerr.New(401, "SESSION_PASSWORD_NEEDED"),
		tgerr.New(401, "AUTH_KEY_PERM_EMPTY"),
		tgerr.New(401, "SOMETHING_NEW"),
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
		tgerr.New(400, "CHANNEL_INVALID"),
		tgerr.New(303, "FILE_MIGRATE_4"),
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
		tgerr.New(401, "SESSION_PASSWORD_NEEDED"),
		tgerr.New(500, "INTERNAL_SERVER_ERROR"),
		errors.New("storage unavailable"),
		context.Canceled,
	} {
		r := &Running{cancel: func() {}}
		r.stop(sessionError(original))
		require.ErrorIs(t, r.Err(), original)
		assert.Equal(t, isSessionRefusal(original) || errors.Is(original, ErrSessionUnauthorized),
			errors.Is(r.Err(), ErrSessionUnauthorized), "%v", original)
	}
}

// TestAnsweredAwayCoversTheRoutableNamespaces pins the methods gotd may send
// to a DC other than the home one: the upload.* and stats.* ones, and no
// other.
func TestAnsweredAwayCoversTheRoutableNamespaces(t *testing.T) {
	for _, away := range []bin.Encoder{
		&tg.UploadGetFileRequest{},
		&tg.UploadGetFileHashesRequest{},
		&tg.UploadGetCDNFileRequest{},
		&tg.UploadReuploadCDNFileRequest{},
		&tg.UploadGetWebFileRequest{},
		&tg.StatsGetBroadcastStatsRequest{},
		&tg.StatsGetMegagroupStatsRequest{},
		&tg.StatsLoadAsyncGraphRequest{},
	} {
		assert.True(t, answeredAway(away), "%T may be answered away from home", away)
	}
	for _, home := range []bin.Encoder{
		&tg.UsersGetUsersRequest{},
		&tg.MessagesGetHistoryRequest{},
		&tg.AuthExportAuthorizationRequest{},
		&tg.ChannelsGetFullChannelRequest{},
	} {
		assert.False(t, answeredAway(home), "%T is answered by the home DC", home)
	}
}

// watched is a serving Running as StartClient builds it, over a scripted
// Telegram: the context its Run loop would run on, and what it logs.
type watched struct {
	*Running
	life context.Context
	logs *bytes.Buffer
	inv  *telegramfake.Invoker
}

// newWatched returns a serving Running whose calls go through its refusal
// watch to a Telegram answering with script.
func newWatched(t *testing.T, script ...telegramfake.InvokeFunc) *watched {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	var logs bytes.Buffer
	r := &Running{lifetime: ctx, cancel: cancel, logger: slog.New(slog.NewTextHandler(&logs, nil))}
	inv := telegramfake.New(script...)
	r.api = tg.NewClient(r.refusalWatch().Handle(inv))
	t.Cleanup(r.checks.Wait)
	return &watched{Running: r, life: ctx, logs: &logs, inv: inv}
}

// download makes one call gotd may send to a DC other than the home one.
func (w *watched) download(ctx context.Context) error {
	_, err := w.api.UploadGetFile(ctx, &tg.UploadGetFileRequest{Location: &tg.InputDocumentFileLocation{}, Limit: 1024})
	return err //nolint:wrapcheck // the test inspects the watch's reply as is.
}

// history makes one call only the home DC answers.
func (w *watched) history(ctx context.Context) error {
	_, err := w.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: &tg.InputPeerSelf{}})
	return err //nolint:wrapcheck // the test inspects the watch's reply as is.
}

// refuseFile answers a download with err, as a secondary DC refusing the
// session does.
func refuseFile(err error) telegramfake.InvokeFunc {
	return telegramfake.Typed(func(context.Context, *tg.UploadGetFileRequest, *tg.UploadFileBox) error { return err })
}

// refuseHistory answers a history call with err, as the home DC does.
func refuseHistory(err error) telegramfake.InvokeFunc {
	return telegramfake.Typed(func(context.Context, *tg.MessagesGetHistoryRequest, *tg.MessagesMessagesBox) error {
		return err
	})
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

// TestRefusalWatchPassesOtherFailures pins that a failure which is no refusal
// neither asks the home DC nor stops the client, whichever DC may have
// answered it.
func TestRefusalWatchPassesOtherFailures(t *testing.T) {
	for _, alive := range []error{errors.New("dial tcp: no route to host"), tgerr.New(420, "FLOOD_WAIT_30"), tgerr.New(400, "CHANNEL_INVALID")} {
		w := newWatched(t, refuseFile(alive), refuseHistory(alive))
		require.ErrorIs(t, w.download(t.Context()), alive)
		require.ErrorIs(t, w.history(t.Context()), alive)
		w.checks.Wait()
		require.NoError(t, w.Err(), "%v must not stop the client", alive)
		require.NoError(t, w.life.Err())
		assert.Len(t, w.inv.RequestTypes(), 2, "%v must not trigger a home-DC check", alive)
	}
}

// TestRefusalWatchTakesAHomeRefusalAsTheVerdict pins that a call only the home
// DC answers, refused, stops the client with the verdict before it returns,
// without asking anything more of Telegram.
func TestRefusalWatchTakesAHomeRefusalAsTheVerdict(t *testing.T) {
	for _, refusal := range []error{
		tgerr.New(401, "AUTH_KEY_UNREGISTERED"),
		tgerr.New(401, "SESSION_REVOKED"),
		tgerr.New(406, "SESSION_EXPIRED"),
		tgerr.New(401, "USER_DEACTIVATED_BAN"),
		// Any 401 of the home DC is the verdict.
		tgerr.New(401, "SESSION_PASSWORD_NEEDED"),
	} {
		w := newWatched(t, refuseHistory(refusal))
		err := w.history(t.Context())
		require.ErrorIs(t, err, refusal, "the caller still sees Telegram's own error")
		require.ErrorIs(t, err, ErrSessionUnauthorized)
		assert.True(t, IsSystemic(err))
		require.ErrorIs(t, w.Err(), ErrSessionUnauthorized)
		require.ErrorIs(t, w.Err(), refusal, "the client keeps the home DC's reply")
		require.Error(t, w.life.Err(), "the Run loop is told to stop")
		assert.Zero(t, w.inv.Remaining(), "nothing more is asked of Telegram")

		// A later reason never overwrites the verdict.
		w.stop(errClosed)
		require.ErrorIs(t, w.Err(), ErrSessionUnauthorized)
	}
}

// TestRefusalWatchChecksASecondaryRefusalInTheBackground pins the secondary-DC
// case. A refused download returns at once, without waiting for the home DC.
// Downloads refused while the check runs return at once too and start
// no other check. The check's answer then decides: the home DC refusing the
// session is the verdict, the home DC accepting it stops the client so that
// reconnecting restores the refused calls, and a check that fails keeps the
// client serving, with the next refused download checking again.
func TestRefusalWatchChecksASecondaryRefusalInTheBackground(t *testing.T) {
	refusal := tgerr.New(401, "AUTH_KEY_UNREGISTERED")
	unreachable := errors.New("dial tcp: i/o timeout")
	for name, home := range map[string]error{
		"home DC refuses the session": tgerr.New(401, "SESSION_REVOKED"),
		"home DC accepts the session": nil,
		"home DC cannot be asked":     unreachable,
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				const refused = 4
				script := []telegramfake.InvokeFunc{
					refuseFile(refusal),
					homeCheck(func(ctx context.Context) error {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-release:
							return home
						}
					}),
				}
				for range refused - 1 {
					script = append(script, refuseFile(refusal))
				}
				w := newWatched(t, script...)

				for range refused {
					err := w.download(t.Context())
					require.ErrorIs(t, err, ErrSecondaryRefusal)
					require.ErrorIs(t, err, refusal, "the caller still sees Telegram's own error")
					assert.NotErrorIs(t, err, ErrSessionUnauthorized, "a secondary DC's refusal is no verdict")
					assert.False(t, IsSystemic(err))
					assert.False(t, IsPeerSpecific(err))
					synctest.Wait() // the one check waits on the home DC
				}
				require.NoError(t, w.Err(), "the client serves while the home DC is asked")
				assert.Zero(t, w.inv.Remaining(), "one check served every refused download")

				close(release)
				w.checks.Wait()
				switch {
				case home == nil:
					require.ErrorIs(t, w.Err(), ErrSecondaryRefusal)
					require.ErrorIs(t, w.Err(), refusal)
					assert.NotErrorIs(t, w.Err(), ErrSessionUnauthorized, "the session stands")
					assert.ErrorContains(t, w.Err(), "reconnecting restores")
					require.Error(t, w.life.Err(), "the Run loop is told to stop")
				case errors.Is(home, unreachable):
					require.NoError(t, w.Err(), "a failed check keeps the client serving")
					require.NoError(t, w.life.Err())
					assert.Contains(t, w.logs.String(), "could not be asked")
					assert.Contains(t, w.logs.String(), unreachable.Error())

					w.inv = telegramfake.New(refuseFile(refusal), homeCheck(func(context.Context) error { return nil }))
					w.api = tg.NewClient(w.refusalWatch().Handle(w.inv))
					require.ErrorIs(t, w.download(t.Context()), ErrSecondaryRefusal)
					w.checks.Wait()
					assert.Zero(t, w.inv.Remaining(), "the next refused download checks again")
					require.ErrorIs(t, w.Err(), ErrSecondaryRefusal)
				default:
					require.ErrorIs(t, w.Err(), ErrSessionUnauthorized)
					require.ErrorIs(t, w.Err(), home, "the client keeps the home DC's reply")
					require.Error(t, w.life.Err(), "the Run loop is told to stop")
				}
			})
		})
	}
}

// TestHomeCheckEndsWithTheClient pins the check's own context: it has a
// deadline, and a client stopping while the check runs ends the check
// without a warning about an unreachable home DC.
func TestHomeCheckEndsWithTheClient(t *testing.T) {
	var w *watched
	w = newWatched(t, refuseFile(tgerr.New(401, "AUTH_KEY_UNREGISTERED")), homeCheck(func(ctx context.Context) error {
		_, bounded := ctx.Deadline()
		assert.True(t, bounded, "the check must be bounded")
		w.stop(errClosed)
		<-ctx.Done()
		return ctx.Err()
	}))
	require.ErrorIs(t, w.download(t.Context()), ErrSecondaryRefusal)
	w.checks.Wait()
	require.ErrorIs(t, w.Err(), errClosed)
	assert.Empty(t, w.logs.String(), "a client stopping is no reason to warn")
}

// TestRefusalWatchAfterTheClientStopped pins that a download refused once the
// client has stopped asks nothing more of the home DC and claims no check,
// and that a home refusal then still leaves the first reason in place.
func TestRefusalWatchAfterTheClientStopped(t *testing.T) {
	refusal := tgerr.New(401, "AUTH_KEY_UNREGISTERED")
	w := newWatched(t, refuseFile(refusal), refuseHistory(refusal))
	w.stop(errClosed)

	err := w.download(t.Context())
	require.ErrorIs(t, err, refusal)
	assert.NotErrorIs(t, err, ErrSecondaryRefusal, "no check runs to answer it")
	require.ErrorIs(t, w.history(t.Context()), ErrSessionUnauthorized)
	require.ErrorIs(t, w.Err(), errClosed, "the first reason stays")
	assert.Zero(t, w.inv.Remaining())
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

// answer is how a scripted DC answers one request.
type answer = func(*tgtest.Server, *tgtest.Request) error

// homeDC is the DC a client with an empty session starts on.
const homeDC = 2

// scripted answers the n-th call with the n-th of answers, and any call past
// them with an error.
func scripted(answers ...answer) answer {
	var mu sync.Mutex
	return func(server *tgtest.Server, req *tgtest.Request) error {
		mu.Lock()
		if len(answers) == 0 {
			mu.Unlock()
			return server.SendErr(req, tgerr.New(500, "UNSCRIPTED_CALL"))
		}
		next := answers[0]
		answers = answers[1:]
		mu.Unlock()
		return next(server, req)
	}
}

// startOnCluster starts a client through startClient against an in-process
// Telegram cluster whose DCs route sets up; the client's home DC is homeDC.
// Every wait of the test is bounded by clusterTimeout.
func startOnCluster(t *testing.T, onFloodWait FloodWaitCallback, route func(*cluster.Cluster)) (*Running, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), clusterTimeout)
	t.Cleanup(cancel)

	c := cluster.NewCluster(cluster.Options{})
	route(c)
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
	case <-ctx.Done():
		t.Fatalf("cluster did not come up: %v", ctx.Err())
	}

	cfg := &Config{APIID: 1, APIHash: "hash", FloodWaitMaxWait: time.Minute}
	r, err := startClient(ctx, cfg, &session.StorageMemory{}, discardLogger(), onFloodWait, telegram.Options{
		PublicKeys: c.Keys(),
		Resolver:   c.Resolver(),
		DCList:     c.List(),
	})
	if r != nil {
		t.Cleanup(r.Close)
	}
	return r, err //nolint:wrapcheck // the test inspects startClient's answer as is.
}

// clusterTimeout bounds every wait of a test against an in-process cluster.
const clusterTimeout = 30 * time.Second

func answerErr(err *tgerr.Error) answer {
	return func(server *tgtest.Server, req *tgtest.Request) error { return server.SendErr(req, err) }
}

// answerErrOnRelease signals asked, then answers with err once release is
// closed. It returns at once, so the DC goes on serving the connection's other
// requests meanwhile.
func answerErrOnRelease(err *tgerr.Error, asked chan<- struct{}, release <-chan struct{}) answer {
	return func(server *tgtest.Server, req *tgtest.Request) error {
		select {
		case asked <- struct{}{}:
		default:
		}
		go func() {
			<-release
			_ = server.SendErr(req, err)
		}()
		return nil
	}
}

func answerSelf(server *tgtest.Server, req *tgtest.Request) error {
	return server.SendVector(req, &tg.User{ID: 7, AccessHash: 70}) //nolint:wrapcheck // the fake server hands the send error to tgtest as is.
}

func answerFile(server *tgtest.Server, req *tgtest.Request) error {
	return server.SendResult(req, &tg.UploadFile{Type: &tg.StorageFileJpeg{}, Bytes: []byte("jpeg")}) //nolint:wrapcheck // the fake server hands the send error to tgtest as is.
}

// TestStartClientWatchesRefusalsInsideFloodWait pins the real client's
// wiring. A download waits out its flood wait, and the refusal its retry gets
// goes through refusalWatch, which returns it at once and has the home DC
// checked. While the home DC holds the check, the next download is served:
// the check holds up no call. The home DC then refuses the session, which
// stops the client with the verdict.
func TestStartClientWatchesRefusalsInsideFloodWait(t *testing.T) {
	t.Parallel()
	var waits atomic.Int32
	asked, release := make(chan struct{}, 1), make(chan struct{})
	// gotd resends a request the DC has not answered within a few seconds,
	// so every copy of the check is held the same way.
	held := answerErrOnRelease(tgerr.New(401, "SESSION_REVOKED"), asked, release)
	r, err := startOnCluster(t, func(context.Context, time.Duration, bool) { waits.Add(1) }, func(c *cluster.Cluster) {
		c.Dispatch(homeDC, "home").
			HandleFunc(tg.UsersGetUsersRequestTypeID, scripted(answerSelf, held, held, held, held)).
			HandleFunc(tg.UploadGetFileRequestTypeID, scripted(answerErr(tgerr.New(420, "FLOOD_WAIT_0")), answerErr(tgerr.New(401, "AUTH_KEY_UNREGISTERED")), answerFile))
	})
	require.NoError(t, err)
	assert.Equal(t, int64(7), r.Self().ID)

	ctx, cancel := context.WithTimeout(t.Context(), clusterTimeout)
	defer cancel()
	download := func(ctx context.Context) error {
		_, err := r.API().UploadGetFile(ctx, &tg.UploadGetFileRequest{Location: &tg.InputDocumentFileLocation{}, Limit: 1024})
		return err //nolint:wrapcheck // the test inspects the client's answer as is.
	}
	err = download(ctx)
	require.ErrorIs(t, err, ErrSecondaryRefusal, "the refusal is returned before the home DC answers the check")
	assert.NotErrorIs(t, err, ErrSessionUnauthorized)
	assert.Equal(t, int32(1), waits.Load(), "the flood-wait middleware retried the call first")

	select {
	case <-asked:
	case <-ctx.Done():
		t.Fatal("the home DC was never asked about the session")
	}
	// A check that held up calls would hold this download until the
	// check timed out; sent beside it, the download takes a local round trip.
	sendCtx, sendCancel := context.WithTimeout(ctx, 3*time.Second)
	defer sendCancel()
	require.NoError(t, download(sendCtx), "calls are served while the check waits on the home DC")
	require.NoError(t, r.Err())

	close(release)
	select {
	case <-r.Done():
	case <-ctx.Done():
		t.Fatal("the client did not stop on the home DC's refusal")
	}
	require.ErrorIs(t, r.Err(), ErrSessionUnauthorized)
	assert.True(t, tgerr.Is(r.Err(), "SESSION_REVOKED"), "the verdict keeps the home DC's code: %v", r.Err())
}

// TestStartClientDownloadsFromAnotherDC pins that a download the home DC
// redirects with FILE_MIGRATE completes on the DC it names. gotd first moves
// the session's authorisation there, with a call of its own that passes
// through the client's middlewares while the download is still inside them:
// a middleware that sends one call only once the one before it has returned
// holds the download until its deadline.
func TestStartClientDownloadsFromAnotherDC(t *testing.T) {
	t.Parallel()
	const fileDC = 4
	always := func(result bin.Encoder) answer {
		return func(server *tgtest.Server, req *tgtest.Request) error {
			return server.SendResult(req, result) //nolint:wrapcheck // the fake server hands the send error to tgtest as is.
		}
	}
	r, err := startOnCluster(t, nil, func(c *cluster.Cluster) {
		c.Dispatch(homeDC, "home").
			HandleFunc(tg.UsersGetUsersRequestTypeID, scripted(answerSelf)).
			HandleFunc(tg.UploadGetFileRequestTypeID, scripted(answerErr(tgerr.New(303, fmt.Sprintf("FILE_MIGRATE_%d", fileDC))))).
			HandleFunc(tg.AuthExportAuthorizationRequestTypeID, always(&tg.AuthExportedAuthorization{ID: 7, Bytes: []byte("auth")}))
		c.Dispatch(fileDC, "files").
			HandleFunc(tg.AuthImportAuthorizationRequestTypeID, always(&tg.AuthAuthorization{User: &tg.User{ID: 7}})).
			HandleFunc(tg.UploadGetFileRequestTypeID, scripted(answerFile))
	})
	require.NoError(t, err)

	// Every answer is a local round trip, so a download that takes seconds
	// is held, not slow.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	file, err := r.API().UploadGetFile(ctx, &tg.UploadGetFileRequest{Location: &tg.InputDocumentFileLocation{}, Limit: 1024})
	require.NoError(t, err, "the download completes on DC %d", fileDC)
	require.IsType(t, &tg.UploadFile{}, file)
	assert.Equal(t, []byte("jpeg"), file.(*tg.UploadFile).Bytes)
	require.NoError(t, r.Err())
}

// TestStartClientKeepsTheHomeDCsRefusal pins that a readiness check the home
// DC refuses — a session refusal or any other 401 — is the verdict and keeps
// Telegram's own code.
func TestStartClientKeepsTheHomeDCsRefusal(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"AUTH_KEY_UNREGISTERED", "SESSION_PASSWORD_NEEDED"} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			_, err := startOnCluster(t, nil, func(c *cluster.Cluster) {
				c.Dispatch(homeDC, "home").HandleFunc(tg.UsersGetUsersRequestTypeID, scripted(answerErr(tgerr.New(401, code))))
			})
			require.ErrorIs(t, err, ErrSessionUnauthorized)
			assert.True(t, tgerr.Is(err, code), "the verdict keeps the home DC's code: %v", err)
		})
	}
}
