package tgclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
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
		lifetime, stop := context.WithCancelCause(t.Context())
		r := &Running{lifetime: lifetime, stop: stop}
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
// Telegram: the context its Run loop would run on, and the Telegram it calls.
type watched struct {
	*Running
	inv *telegramfake.Invoker
}

// newWatched returns a serving Running whose calls go through its stop and
// refusal watches to a Telegram answering with script.
func newWatched(t *testing.T, script ...telegramfake.InvokeFunc) *watched {
	lifetime, stop := context.WithCancelCause(t.Context())
	t.Cleanup(func() { stop(errClosed) })
	r := &Running{lifetime: lifetime, stop: stop}
	inv := telegramfake.New(script...)
	r.api = tg.NewClient(r.stopWatch(r.refusalWatch().Handle(inv)))
	return &watched{Running: r, inv: inv}
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

// refuseFile answers a download with err, as a DC other than the home one
// refusing the session does.
func refuseFile(err error) telegramfake.InvokeFunc {
	return telegramfake.Typed(func(context.Context, *tg.UploadGetFileRequest, *tg.UploadFileBox) error { return err })
}

// refuseHistory answers a history call with err, as the home DC does.
func refuseHistory(err error) telegramfake.InvokeFunc {
	return telegramfake.Typed(func(context.Context, *tg.MessagesGetHistoryRequest, *tg.MessagesMessagesBox) error {
		return err
	})
}

// TestRefusalWatchPassesOtherFailures pins that a failure which is no refusal
// neither stops the client nor changes, whichever DC may have answered it.
func TestRefusalWatchPassesOtherFailures(t *testing.T) {
	for _, alive := range []error{errors.New("dial tcp: no route to host"), tgerr.New(420, "FLOOD_WAIT_30"), tgerr.New(400, "CHANNEL_INVALID")} {
		w := newWatched(t, refuseFile(alive), refuseHistory(alive))
		require.ErrorIs(t, w.download(t.Context()), alive)
		require.ErrorIs(t, w.history(t.Context()), alive)
		require.NoError(t, w.Err(), "%v must not stop the client", alive)
		assert.Len(t, w.inv.RequestTypes(), 2, "%v must ask nothing more of Telegram", alive)
	}
}

// TestRefusalWatchTakesAHomeRefusalAsTheVerdict pins that a call only the home
// DC answers, refused, stops the client with the verdict before it returns,
// without asking anything more of Telegram, and fails as a call on a stopped
// client.
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
		require.ErrorIs(t, err, ErrClientStopped)
		require.ErrorIs(t, err, ErrSessionUnauthorized)
		require.ErrorIs(t, err, refusal, "the caller still sees Telegram's own error")
		assert.True(t, IsSystemic(err))
		require.ErrorIs(t, w.Err(), ErrSessionUnauthorized)
		require.ErrorIs(t, w.Err(), refusal, "the client keeps the home DC's reply")
		assert.Zero(t, w.inv.Remaining(), "nothing more is asked of Telegram")

		// A later reason never overwrites the verdict.
		w.stop(errClosed)
		require.ErrorIs(t, w.Err(), ErrSessionUnauthorized)
	}
}

// TestRefusalWatchReconnectsOnAnAwayRefusal pins that a call another DC may
// have answered, refused, is no verdict on the session: the client stops with
// a reason that asks its owner to reconnect, whose readiness check has the
// home DC judge the session, and the call fails as a call on a stopped client.
func TestRefusalWatchReconnectsOnAnAwayRefusal(t *testing.T) {
	refusal := tgerr.New(401, "AUTH_KEY_UNREGISTERED")
	w := newWatched(t, refuseFile(refusal))

	err := w.download(t.Context())
	require.ErrorIs(t, err, ErrClientStopped)
	require.ErrorIs(t, err, errAwayRefusal)
	require.ErrorIs(t, err, refusal, "the caller still sees Telegram's own error")
	assert.NotErrorIs(t, err, ErrSessionUnauthorized, "another DC's refusal is no verdict")
	assert.True(t, IsSystemic(err), "a batch stops with the client")
	assert.False(t, IsPeerSpecific(err))
	require.ErrorIs(t, w.Err(), errAwayRefusal)
	assert.NotErrorIs(t, w.Err(), ErrSessionUnauthorized)
	assert.ErrorContains(t, w.Err(), "reconnecting has the home DC judge the session")
	assert.Zero(t, w.inv.Remaining(), "nothing more is asked of Telegram")
}

// TestStopWatchNamesWhyTheClientStopped pins that a call failing once the
// client has stopped fails with why, not with the cancellation or closed
// connection gotd's shutdown left it with — which would pass for the
// caller's own cancellation — and that the first reason stays.
func TestStopWatchNamesWhyTheClientStopped(t *testing.T) {
	refusal := tgerr.New(401, "AUTH_KEY_UNREGISTERED")
	dropped := errors.New("read tcp: connection reset by peer")
	var w *watched
	w = newWatched(t,
		refuseHistory(tgerr.New(400, "CHANNEL_INVALID")),
		telegramfake.Typed(func(context.Context, *tg.MessagesGetHistoryRequest, *tg.MessagesMessagesBox) error {
			w.stop(dropped)
			return context.Canceled
		}),
		refuseFile(refusal),
		refuseHistory(refusal),
	)

	require.True(t, tgerr.Is(w.history(t.Context()), "CHANNEL_INVALID"), "a serving client passes Telegram's reply through")

	err := w.history(t.Context())
	require.ErrorIs(t, err, ErrClientStopped)
	require.ErrorIs(t, err, dropped)
	assert.NotErrorIs(t, err, context.Canceled, "the caller did not cancel")

	for _, call := range []func(context.Context) error{w.download, w.history} {
		err := call(t.Context())
		require.ErrorIs(t, err, ErrClientStopped)
		require.ErrorIs(t, err, dropped, "the first reason stays")
		assert.NotErrorIs(t, err, ErrSessionUnauthorized)
	}
	require.ErrorIs(t, w.Err(), dropped)
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

	_, err := StartClient(t.Context(), cfg, loadFailingStorage{err: tgerr.New(401, "AUTH_KEY_UNREGISTERED")}, discardLogger())
	require.ErrorIs(t, err, ErrSessionUnauthorized)

	storageErr := errors.New("keychain access denied")
	_, err = StartClient(t.Context(), cfg, loadFailingStorage{err: storageErr}, discardLogger())
	require.ErrorIs(t, err, storageErr)
	assert.NotErrorIs(t, err, ErrSessionUnauthorized)
}

// TestStartClientCancelledIsNotAVerdict pins that a startup cut short by its
// caller reports the cancellation, never a dead session.
func TestStartClientCancelledIsNotAVerdict(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := StartClient(ctx, &Config{APIID: 1, APIHash: "hash"}, blockingStorage{}, discardLogger())
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
func startOnCluster(t *testing.T, logger *slog.Logger, route func(*cluster.Cluster)) (*Running, error) {
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
	r, err := startClient(ctx, cfg, &session.StorageMemory{}, logger, telegram.Options{
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

func answerSelf(server *tgtest.Server, req *tgtest.Request) error {
	return server.SendVector(req, &tg.User{ID: 7, AccessHash: 70}) //nolint:wrapcheck // the fake server hands the send error to tgtest as is.
}

func answerFile(server *tgtest.Server, req *tgtest.Request) error {
	return server.SendResult(req, &tg.UploadFile{Type: &tg.StorageFileJpeg{}, Bytes: []byte("jpeg")}) //nolint:wrapcheck // the fake server hands the send error to tgtest as is.
}

// lockedBuffer is a log sink the client's goroutines may write to while the
// test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p) //nolint:wrapcheck // a bytes.Buffer write never fails.
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestStartClientWatchesRefusalsInsideFloodWait pins the real client's
// wiring. A download waits out its flood wait, logged through the client's
// logger, and the refusal its retry gets goes through refusalWatch: another
// DC may have answered it, so the client stops for a reconnect, and the
// download fails with why through stopWatch.
func TestStartClientWatchesRefusalsInsideFloodWait(t *testing.T) {
	t.Parallel()
	var logs lockedBuffer
	r, err := startOnCluster(t, slog.New(slog.NewTextHandler(&logs, nil)), func(c *cluster.Cluster) {
		c.Dispatch(homeDC, "home").
			HandleFunc(tg.UsersGetUsersRequestTypeID, scripted(answerSelf)).
			HandleFunc(tg.UploadGetFileRequestTypeID, scripted(answerErr(tgerr.New(420, "FLOOD_WAIT_0")), answerErr(tgerr.New(401, "AUTH_KEY_UNREGISTERED"))))
	})
	require.NoError(t, err)
	assert.Equal(t, int64(7), r.Self().ID)

	ctx, cancel := context.WithTimeout(t.Context(), clusterTimeout)
	defer cancel()
	_, err = r.API().UploadGetFile(ctx, &tg.UploadGetFileRequest{Location: &tg.InputDocumentFileLocation{}, Limit: 1024})
	require.ErrorIs(t, err, ErrClientStopped)
	require.ErrorIs(t, err, errAwayRefusal)
	assert.True(t, tgerr.Is(err, "AUTH_KEY_UNREGISTERED"), "the call keeps Telegram's code: %v", err)
	assert.NotErrorIs(t, err, ErrSessionUnauthorized, "another DC's refusal is no verdict")
	assert.Contains(t, logs.String(), "telegram told a call to wait; waiting it out", "the flood-wait middleware retried the call first")

	select {
	case <-r.Done():
	case <-ctx.Done():
		t.Fatal("the client did not stop on the refusal")
	}
	require.ErrorIs(t, r.Err(), errAwayRefusal)
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
	r, err := startOnCluster(t, discardLogger(), func(c *cluster.Cluster) {
		c.Dispatch(homeDC, "home").
			HandleFunc(tg.UsersGetUsersRequestTypeID, scripted(answerSelf)).
			HandleFunc(tg.UploadGetFileRequestTypeID, scripted(answerErr(tgerr.New(303, fmt.Sprintf("FILE_MIGRATE_%d", fileDC))))).
			HandleFunc(tg.AuthExportAuthorizationRequestTypeID, always(&tg.AuthExportedAuthorization{ID: 7, Bytes: []byte("auth")}))
		c.Dispatch(fileDC, "files").
			HandleFunc(tg.AuthImportAuthorizationRequestTypeID, always(&tg.AuthAuthorization{User: &tg.User{ID: 7}})).
			HandleFunc(tg.UploadGetFileRequestTypeID, scripted(answerFile))
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), clusterTimeout)
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
			_, err := startOnCluster(t, discardLogger(), func(c *cluster.Cluster) {
				c.Dispatch(homeDC, "home").HandleFunc(tg.UsersGetUsersRequestTypeID, scripted(answerErr(tgerr.New(401, code))))
			})
			require.ErrorIs(t, err, ErrSessionUnauthorized)
			assert.True(t, tgerr.Is(err, code), "the verdict keeps the home DC's code: %v", err)
		})
	}
}
