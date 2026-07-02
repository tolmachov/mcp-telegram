package resources

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/messages"
)

// fakeChat is a minimal pinned chat: a user peer with an ID and display name.
type fakeChat struct {
	id   int64
	name string
}

// pinnedInvoker fakes tg.Invoker for messages.getPinnedDialogs. It returns the
// configured chats as pinned user dialogs, or a canned error. chats can be
// swapped between refreshes to simulate a reorder or a rename.
type pinnedInvoker struct {
	mu    sync.Mutex
	chats []fakeChat
	err   error
	calls int
}

func (p *pinnedInvoker) set(chats ...fakeChat) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chats = chats
}

func (p *pinnedInvoker) setErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

func (p *pinnedInvoker) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *pinnedInvoker) Invoke(_ context.Context, _ bin.Encoder, out bin.Decoder) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return p.err
	}
	dst, ok := out.(*tg.MessagesPeerDialogs)
	if !ok {
		return fmt.Errorf("pinnedInvoker: unexpected output type %T", out)
	}
	res := &tg.MessagesPeerDialogs{}
	for _, c := range p.chats {
		res.Users = append(res.Users, &tg.User{ID: c.id, FirstName: c.name})
		res.Dialogs = append(res.Dialogs, &tg.Dialog{
			Peer:   &tg.PeerUser{UserID: c.id},
			Pinned: true,
		})
	}
	*dst = *res
	return nil
}

// syncBuffer is a mutex-guarded buffer so a slog handler writing from the
// watcher goroutine and a test reading concurrently don't race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

func newTestProvider(inv *pinnedInvoker, logger *slog.Logger, nServers int) (*PinnedChatsProvider, []*mcp.Server) {
	api := tg.NewClient(inv)
	msgProvider := messages.NewProviderWithRate(api, 0)
	servers := make([]*mcp.Server, nServers)
	for i := range servers {
		servers[i] = mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	}
	return NewPinnedChatsProvider(api, msgProvider, logger, servers...), servers
}

// listResources connects an in-memory client to srv and returns uri → name for
// every registered resource, following pagination.
func listResources(t *testing.T, srv *mcp.Server) map[string]string {
	t.Helper()
	ctx := context.Background()

	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	require.NoError(t, err)
	defer func() { _ = ss.Close() }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	require.NoError(t, err)
	defer func() { _ = cs.Close() }()

	out := map[string]string{}
	params := &mcp.ListResourcesParams{}
	for {
		res, err := cs.ListResources(ctx, params)
		require.NoError(t, err)
		for _, r := range res.Resources {
			out[r.URI] = r.Name
		}
		if res.NextCursor == "" {
			break
		}
		params.Cursor = res.NextCursor
	}
	return out
}

// TestPinnedChatsMirroredOntoAllServers is the core cross-feature contract of
// SEP-2053 + pinned resources: the single poller must register the pinned set on
// EVERY inner server so the compact and research variants can list/read pinned
// chats too, not just the full variant. A regression registering onto only the
// first server would leave the others empty and no tools-only test would catch
// it.
func TestPinnedChatsMirroredOntoAllServers(t *testing.T) {
	inv := &pinnedInvoker{}
	inv.set(fakeChat{id: 111, name: "Alice"}, fakeChat{id: 222, name: "Bob"})
	p, servers := newTestProvider(inv, quietLogger(), 3)

	require.NoError(t, p.RefreshResources(context.Background()))

	wantURIs := []string{
		"telegram://chats/111/messages",
		"telegram://chats/222/messages",
	}
	for i, srv := range servers {
		got := listResources(t, srv)
		for _, uri := range wantURIs {
			assert.Containsf(t, got, uri, "server %d must expose pinned resource %s", i, uri)
		}
		assert.Lenf(t, got, len(wantURIs), "server %d must expose exactly the pinned set", i)
	}
}

// TestPinnedRefreshReorderIsNoOp guards the sorted-fingerprint short-circuit:
// Telegram returning the same pinned chats in a different order must not churn
// the registry (which would spam resources/list_changed on every tick). We
// observe this white-box: on a no-op refresh currentURIs is never reassigned, so
// it still holds the ORIGINAL Telegram order even though the second response
// reversed it.
func TestPinnedRefreshReorderIsNoOp(t *testing.T) {
	inv := &pinnedInvoker{}
	a := fakeChat{id: 111, name: "Alice"}
	b := fakeChat{id: 222, name: "Bob"}
	inv.set(a, b)
	p, _ := newTestProvider(inv, quietLogger(), 1)

	require.NoError(t, p.RefreshResources(context.Background()))
	firstOrder := append([]string(nil), p.currentURIs...)
	require.Equal(t, []string{
		"telegram://chats/111/messages",
		"telegram://chats/222/messages",
	}, firstOrder)

	// Same chats, reversed order: must short-circuit, leaving currentURIs as-is.
	inv.set(b, a)
	require.NoError(t, p.RefreshResources(context.Background()))
	assert.Equal(t, firstOrder, p.currentURIs,
		"reorder-only refresh must be a no-op (currentURIs not reassigned)")
}

// TestPinnedRefreshReregistersOnRename verifies the fingerprint keys on the chat
// name, not just the URI: a pinned chat that is renamed keeps its ID/URI but
// must re-register so clients see the fresh title instead of a stale one.
func TestPinnedRefreshReregistersOnRename(t *testing.T) {
	inv := &pinnedInvoker{}
	inv.set(fakeChat{id: 111, name: "Alice"})
	p, servers := newTestProvider(inv, quietLogger(), 1)

	require.NoError(t, p.RefreshResources(context.Background()))
	before := listResources(t, servers[0])
	require.Equal(t, "Messages from Alice", before["telegram://chats/111/messages"])

	inv.set(fakeChat{id: 111, name: "Alice Renamed"})
	require.NoError(t, p.RefreshResources(context.Background()))
	after := listResources(t, servers[0])
	assert.Equal(t, "Messages from Alice Renamed", after["telegram://chats/111/messages"],
		"a rename (same URI) must re-register the resource with the new name")
}

// TestPinnedRefreshUnpinRemovesResource guards the RemoveResources cleanup path:
// when a chat is unpinned the set shrinks, and its resource must disappear from
// every mirrored server. The reorder/rename tests only exercise a same-size set
// (the URI never vanishes there), so without this a regression that dropped or
// mis-diffed the removal loop would leave an unpinned chat listed as a live
// resource forever, and no other test would catch it.
func TestPinnedRefreshUnpinRemovesResource(t *testing.T) {
	inv := &pinnedInvoker{}
	alice := fakeChat{id: 111, name: "Alice"}
	bob := fakeChat{id: 222, name: "Bob"}
	inv.set(alice, bob)
	p, servers := newTestProvider(inv, quietLogger(), 2)

	require.NoError(t, p.RefreshResources(context.Background()))
	for i, srv := range servers {
		got := listResources(t, srv)
		require.Containsf(t, got, "telegram://chats/222/messages", "server %d must start with Bob pinned", i)
	}

	// Unpin Bob: the set shrinks to just Alice.
	inv.set(alice)
	require.NoError(t, p.RefreshResources(context.Background()))

	for i, srv := range servers {
		got := listResources(t, srv)
		assert.Containsf(t, got, "telegram://chats/111/messages", "server %d must keep Alice", i)
		assert.NotContainsf(t, got, "telegram://chats/222/messages",
			"server %d must drop the unpinned Bob resource", i)
		assert.Lenf(t, got, 1, "server %d must expose exactly the shrunk set", i)
	}
}

// TestRefreshResourcesWrapsError confirms a Telegram failure surfaces as a
// wrapped error rather than a silently frozen resource set.
func TestRefreshResourcesWrapsError(t *testing.T) {
	inv := &pinnedInvoker{}
	inv.setErr(errors.New("boom"))
	p, _ := newTestProvider(inv, quietLogger(), 1)

	err := p.RefreshResources(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refreshing pinned resources")
	assert.Contains(t, err.Error(), "boom")
}

// TestNewPinnedChatsProviderWarnsWithoutServers covers the guard that a provider
// built with no servers logs loudly: it would otherwise fetch from Telegram and
// register the results onto nobody.
func TestNewPinnedChatsProviderWarnsWithoutServers(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	api := tg.NewClient(&pinnedInvoker{})
	msgProvider := messages.NewProviderWithRate(api, 0)

	NewPinnedChatsProvider(api, msgProvider, logger) // no servers

	assert.Contains(t, buf.String(), "level=WARN")
	assert.Contains(t, buf.String(), "no servers")
}

// TestWatchInBackgroundInitialFailureLogsError pins the documented severity
// split: an initial refresh failure means startup is broken from the first tick,
// so it must log at Error (operator alerting keys on that), not Warn. A long
// interval keeps the periodic ticker from firing, so only the initial refresh
// runs before we cancel.
func TestWatchInBackgroundInitialFailureLogsError(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	inv := &pinnedInvoker{}
	inv.setErr(errors.New("auth expired"))
	p, _ := newTestProvider(inv, logger, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := p.WatchInBackground(ctx, time.Hour)

	require.Eventually(t, func() bool {
		return strings.Contains(buf.String(), "level=ERROR")
	}, 2*time.Second, 10*time.Millisecond, "initial refresh failure must log at Error")

	cancel()
	<-done

	logs := buf.String()
	assert.Contains(t, logs, "initial refresh failed")
	assert.NotContains(t, logs, "level=WARN", "initial failure must be Error, not Warn")
}

// TestWatchInBackgroundZeroIntervalDisables pins the documented "0 disables the
// watcher" contract: it must return an already-closed channel, never call
// Telegram, and never reach time.NewTicker (which panics on a non-positive
// interval). A regression that moved the ticker construction above the guard, or
// dropped the guard, would crash at startup for anyone who set
// --pinned-refresh-seconds 0.
func TestWatchInBackgroundZeroIntervalDisables(t *testing.T) {
	inv := &pinnedInvoker{}
	inv.set(fakeChat{id: 111, name: "Alice"})
	p, _ := newTestProvider(inv, quietLogger(), 1)

	done := p.WatchInBackground(context.Background(), 0)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("zero interval must return an already-closed channel")
	}
	assert.Zero(t, inv.callCount(), "disabled watcher must not poll Telegram")
}

// TestWatchInBackgroundNegativeIntervalWarns covers the other half of the
// non-positive split: a negative interval can only be a config typo, so unlike
// the intentional 0 it must warn loudly instead of disabling in silence — while
// still not polling Telegram or panicking on time.NewTicker.
func TestWatchInBackgroundNegativeIntervalWarns(t *testing.T) {
	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	inv := &pinnedInvoker{}
	inv.set(fakeChat{id: 111, name: "Alice"})
	p, _ := newTestProvider(inv, logger, 1)

	done := p.WatchInBackground(context.Background(), -30*time.Second)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("negative interval must return an already-closed channel")
	}
	assert.Zero(t, inv.callCount(), "disabled watcher must not poll Telegram")
	logs := buf.String()
	assert.Contains(t, logs, "level=WARN", "a negative interval must warn, not disable silently")
	assert.Contains(t, logs, "negative")
}
