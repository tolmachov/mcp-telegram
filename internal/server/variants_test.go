package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// noWire stands in for the resources/prompts wiring the tool-listing tests
// do not exercise.
func noWire(*mcp.Server) {}

// noopInvoker satisfies tg.Invoker without a network. Listing tools never calls
// Telegram, so Invoke should never run; it errors loudly if it somehow does.
type noopInvoker struct{}

func (noopInvoker) Invoke(context.Context, bin.Encoder, bin.Decoder) error {
	return fmt.Errorf("noopInvoker: unexpected Telegram call during tool listing")
}

// listToolNames connects an in-memory client to srv and returns every tool's
// name → description, following pagination.
func listToolNames(t *testing.T, srv *mcp.Server) map[string]string {
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
	params := &mcp.ListToolsParams{}
	for {
		res, err := cs.ListTools(ctx, params)
		require.NoError(t, err)
		for _, tl := range res.Tools {
			out[tl.Name] = tl.Description
		}
		if res.NextCursor == "" {
			break
		}
		params.Cursor = res.NextCursor
	}
	return out
}

// listModeTools lists the tools the server for mode serves over stdio, built
// with a fake Telegram client. Registration (AddTool) never touches the
// client, so the no-op invoker is fine.
func listModeTools(t *testing.T, mode serveMode) map[string]string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	api := tg.NewClient(noopInvoker{})
	s := &Server{logger: slog.New(slog.DiscardHandler), opts: Options{MediaMaxBytes: 1024, Transport: TransportStdio}, mode: mode}
	peers := tgclient.NewResolver(t.Context(), api)
	handlers := s.buildHandlers(api, peers, messages.NewProvider(peers, 100_000), tgdata.NewChatsCache(t.Context(), nil))
	impl := &mcp.Implementation{Name: "mcp-telegram", Version: "test"}
	return listToolNames(t, s.newModeServer(impl, nil, handlers, noWire, slog.New(slog.DiscardHandler)))
}

func TestVariantHandlerSplit(t *testing.T) {
	fullNames := listModeTools(t, modeFull)
	researchNames := listModeTools(t, modeResearch)

	assert.Len(t, fullNames, 29, "full variant exposes every tool")
	assert.Len(t, listModeTools(t, modeCompact), 29, "compact keeps every tool")
	assert.Len(t, researchNames, 15, "research variant excludes local filesystem writes")
	assert.NotContains(t, researchNames, "BackupMessages")

	// research must be a strict subset of full.
	for name := range researchNames {
		_, ok := fullNames[name]
		assert.Truef(t, ok, "research tool %q missing from full", name)
	}

	// The mutating/admin tools must not leak into the research variant.
	mutating := []string{
		"SendMessage", "MarkAsRead", "EditMessage", "DeleteMessages", "ForwardMessage",
		"SetReaction", "JoinChat", "LeaveChat", "SetChatMute",
		"CreateFolder", "DeleteFolder", "AddChatsToFolder", "RemoveChatsFromFolder",
	}
	for _, name := range mutating {
		_, inResearch := researchNames[name]
		assert.Falsef(t, inResearch, "mutating tool %q must be excluded from research", name)
		_, inFull := fullNames[name]
		assert.Truef(t, inFull, "mutating tool %q must be present in full", name)
	}
}

// TestVariantDescriptions pins that compact and research serve first-sentence
// descriptions and full serves them whole.
func TestVariantDescriptions(t *testing.T) {
	fullNames := listModeTools(t, modeFull)
	for _, mode := range []serveMode{modeCompact, modeResearch} {
		names := listModeTools(t, mode)
		require.Contains(t, names, "GetChats")
		// GetChats has a multi-sentence description.
		assert.Less(t, len(names["GetChats"]), len(fullNames["GetChats"]), "mode %d should shorten GetChats", mode)
		for name, desc := range names {
			assert.Equalf(t, compactDesc(fullNames[name]), desc, "mode %d: %s", mode, name)
		}
	}
}

func TestNewRejectsInvalidVariant(t *testing.T) {
	cfg := &tgclient.Config{APIID: 1, APIHash: "x"}
	_, err := New(Options{Config: cfg, Variant: "bogus", Transport: TransportStdio, ErrOut: io.Discard})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown variant")
	// The error must enumerate the accepted values (derived from variantDefs).
	assert.Contains(t, err.Error(), "full, compact, research")

	for _, v := range []string{"", "full", "compact", "research"} {
		_, err := New(Options{Config: cfg, Variant: v, Transport: TransportStdio, ErrOut: io.Discard})
		require.NoErrorf(t, err, "variant %q should be accepted", v)
	}
}

func TestModeForVariant(t *testing.T) {
	for id, want := range map[string]serveMode{"": modeFull, "full": modeFull, "compact": modeCompact, "research": modeResearch} {
		mode, ok := modeForVariant(id)
		assert.Truef(t, ok, "%q should be valid", id)
		assert.Equalf(t, want, mode, "%q", id)
	}
	for _, id := range []string{"Full", "monitoring", "read-only", "x"} {
		_, ok := modeForVariant(id)
		assert.Falsef(t, ok, "%q should be invalid", id)
	}
}

// TestVariantDefsTableInvariants guards the table: IDs must be unique and each
// row's mode must match its ID.
func TestVariantDefsTableInvariants(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range variantDefs {
		assert.Falsef(t, seen[d.id], "duplicate variant ID %q in variantDefs", d.id)
		seen[d.id] = true
	}
	wantMode := map[string]serveMode{
		variantFull:     modeFull,
		variantCompact:  modeCompact,
		variantResearch: modeResearch,
	}
	for _, d := range variantDefs {
		assert.Equalf(t, wantMode[d.id], d.mode, "variant %q has unexpected serveMode", d.id)
	}
}

// TestServeModeFlags documents the mode → behaviour mapping that
// buildHandlers and newModeServer branch on.
func TestServeModeFlags(t *testing.T) {
	assert.False(t, modeFull.compacts())
	assert.False(t, modeFull.researchOnly())
	assert.True(t, modeCompact.compacts())
	assert.False(t, modeCompact.researchOnly())
	assert.True(t, modeResearch.compacts(), "research is always compact by construction")
	assert.True(t, modeResearch.researchOnly())
}

func TestCompactDesc(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"two sentences", "Do the thing. Then explain more here.", "Do the thing."},
		{"single sentence", "Just one sentence with no follow-up", "Just one sentence with no follow-up"},
		{"single sentence with period", "Just one sentence.", "Just one sentence."},
		{"abbreviation not a boundary", "Set a goal (e.g. key points) now. More detail.", "Set a goal (e.g. key points) now."},
		{"decimal not a boundary", "Version 3.14 is required. Upgrade first.", "Version 3.14 is required."},
		{"ellipsis in uri not a boundary", "Use telegram://media/... from GetMessages. Returns image.", "Use telegram://media/... from GetMessages."},
		{"question mark boundary", "What broke? Investigate the logs.", "What broke?"},
		{"exclamation boundary", "Do it now! Then verify.", "Do it now!"},
		{"trims surrounding space", "  Trim me. Second.  ", "Trim me."},
		{"lowercase next is not a boundary", "ends here. then lowercase continues", "ends here. then lowercase continues"},
		{"empty string", "", ""},
		{"whitespace only", "   \t\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, compactDesc(tc.in))
		})
	}
}

// TestCompactMiddlewarePassesThroughUnexpectedResult covers the defensive
// branch that exists to make a future SDK shape change loud: a non
// *mcp.ListToolsResult must be logged at Error and returned untouched, not
// dropped or panicked on.
func TestCompactMiddlewarePassesThroughUnexpectedResult(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	stub := &mcp.CallToolResult{} // implements mcp.Result but isn't a ListToolsResult
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return stub, nil
	}
	handler := compactToolsMiddleware(logger)(next)

	got, err := handler(context.Background(), methodListTools, nil)
	require.NoError(t, err)
	gotStub, ok := got.(*mcp.CallToolResult)
	require.True(t, ok, "unexpected result type must pass through unchanged")
	assert.Same(t, stub, gotStub)

	logs := buf.String()
	assert.Contains(t, logs, "level=ERROR", "unexpected result type must log at Error")
	assert.Contains(t, logs, "not *mcp.ListToolsResult")
}

// TestCompactMiddlewareIgnoresNonListMethods confirms the middleware only touches
// tools/list: any other method's result flows through without inspection.
func TestCompactMiddlewareIgnoresNonListMethods(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	stub := &mcp.CallToolResult{}
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return stub, nil
	}
	handler := compactToolsMiddleware(logger)(next)

	got, err := handler(context.Background(), "resources/list", nil)
	require.NoError(t, err)
	gotStub, ok := got.(*mcp.CallToolResult)
	require.True(t, ok)
	assert.Same(t, stub, gotStub, "non-tools/list results must pass through untouched")
}

// TestCompactMiddlewarePassesThroughNilResult covers the branch that treats a
// successful tools/list with a typed-nil *mcp.ListToolsResult as a legitimate
// empty response: it must pass the nil through untouched (no panic, no
// reconstruction into an empty-but-non-nil result), distinct from the
// wrong-type Error branch above.
func TestCompactMiddlewarePassesThroughNilResult(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	var nilRes *mcp.ListToolsResult // typed nil: assertion succeeds, value is nil
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return nilRes, nil
	}
	handler := compactToolsMiddleware(logger)(next)

	got, err := handler(context.Background(), methodListTools, nil)
	require.NoError(t, err)
	gotTyped, ok := got.(*mcp.ListToolsResult)
	require.True(t, ok, "typed-nil ListToolsResult must pass through as itself")
	assert.Nil(t, gotTyped, "nil result must pass through unchanged, not be reconstructed")
}

// TestBackupMessagesOfferedOnlyOnStdioOutsideResearch pins the one place that
// decides whether BackupMessages exists and where it may write.
func TestBackupMessagesOfferedOnlyOnStdioOutsideResearch(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	api := tg.NewClient(noopInvoker{})
	peers := tgclient.NewResolver(t.Context(), api)
	impl := &mcp.Implementation{Name: "mcp-telegram", Version: "test"}

	cases := []struct {
		transport string
		mode      serveMode
		want      bool
	}{
		{TransportStdio, modeFull, true},
		{TransportStdio, modeCompact, true},
		{TransportStdio, modeResearch, false},
		{TransportHTTP, modeFull, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%d", tc.transport, tc.mode), func(t *testing.T) {
			s := &Server{logger: slog.New(slog.DiscardHandler), opts: Options{Transport: tc.transport}, mode: tc.mode}
			handlers := s.buildHandlers(api, peers, messages.NewProvider(peers, 100_000), tgdata.NewChatsCache(t.Context(), nil))
			_, ok := listToolNames(t, s.newModeServer(impl, nil, handlers, noWire, slog.New(slog.DiscardHandler)))["BackupMessages"]
			assert.Equal(t, tc.want, ok)
		})
	}

	s := &Server{logger: slog.New(slog.DiscardHandler)}
	assert.Equal(t, []string{filepath.Join(stateHome, "mcp-telegram", "backups")}, s.backupAllowedPaths())
	s.opts.AllowedPaths = []string{"/explicit"}
	assert.Equal(t, []string{"/explicit"}, s.backupAllowedPaths())
}
