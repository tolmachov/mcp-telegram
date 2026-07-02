package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/experimental-ext-variants/go/sdk/variants"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/summarize"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tools"
)

// testLogger discards output so test runs stay quiet.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

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

// buildTestHandlers constructs the full and research handler lists with a fake
// Telegram client. Registration (AddTool) never touches the client, so the
// no-op invoker is fine.
func buildTestHandlers(t *testing.T) (full, research []tools.Handler) {
	t.Helper()
	api := tg.NewClient(noopInvoker{})
	s := &Server{summarizeCfg: summarize.Config{}, mediaMaxBytes: 1024}
	msgProvider := messages.NewProviderWithRate(api, 0)
	chatsCache := tools.NewChatsCache(api)
	return s.buildHandlers(api, msgProvider, chatsCache)
}

func TestVariantHandlerSplit(t *testing.T) {
	full, research := buildTestHandlers(t)
	impl := &mcp.Implementation{Name: "mcp-telegram", Version: "test"}

	fullNames := listToolNames(t, newInner(impl, nil, full, nil, false, testLogger()))
	researchNames := listToolNames(t, newInner(impl, nil, research, nil, false, testLogger()))

	assert.Len(t, fullNames, 29, "full variant exposes every tool")
	assert.Len(t, researchNames, 16, "research variant exposes the read-only subset")

	// research must be a strict subset of full.
	for name := range researchNames {
		_, ok := fullNames[name]
		assert.Truef(t, ok, "research tool %q missing from full", name)
	}

	// The mutating/admin tools must not leak into the research variant.
	mutating := []string{
		"SendMessage", "MarkAsRead", "EditMessage", "DeleteMessage", "ForwardMessage",
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

func TestBuildVariantsServerMetadata(t *testing.T) {
	full, research := buildTestHandlers(t)
	impl := &mcp.Implementation{Name: "mcp-telegram", Version: "test"}

	vs, inners := buildVariantsServer(impl, nil, full, research, nil, testLogger())
	require.Len(t, inners, 3, "one inner server per variant")

	got := vs.Variants()
	require.Len(t, got, 3)

	// Priority order and metadata are the load-bearing invariants: full must be
	// priority 0 (the default non-negotiating clients receive), research must be
	// experimental, and the hints drive client selection.
	byID := map[string]variants.ServerVariant{}
	for _, v := range got {
		byID[v.ID] = v
	}
	require.Contains(t, byID, "full")
	require.Contains(t, byID, "compact")
	require.Contains(t, byID, "research")

	assert.Equal(t, 0, byID["full"].Priority())
	assert.Equal(t, 1, byID["compact"].Priority())
	assert.Equal(t, 2, byID["research"].Priority())

	assert.Equal(t, variants.Stable, byID["full"].Status)
	assert.Equal(t, variants.Stable, byID["compact"].Status)
	assert.Equal(t, variants.Experimental, byID["research"].Status)

	assert.Equal(t, "human-assistant", byID["full"].Hints[variants.HintUseCase])
	assert.Equal(t, "compact", byID["research"].Hints[variants.HintContextSize])

	// Default ranking (empty hints) must put full first — this is what a client
	// with no variant support silently receives.
	ranked := vs.RankedVariants(context.Background(), variants.VariantHints{})
	require.NotEmpty(t, ranked)
	assert.Equal(t, "full", ranked[0].ID, "full must rank first for non-negotiating clients")

	// The assembly must wire handlers to the right variant: inners[0] (full) has
	// all tools, inners[2] (research) has the read-only subset and no mutating
	// tools. This guards the whole recipe, not just newInner in isolation.
	fullNames := listToolNames(t, inners[0])
	researchNames := listToolNames(t, inners[2])
	assert.Len(t, fullNames, 29)
	assert.Len(t, researchNames, 16)
	for _, name := range []string{"SendMessage", "DeleteMessage", "ForwardMessage", "MarkAsRead", "CreateFolder"} {
		_, leaked := researchNames[name]
		assert.Falsef(t, leaked, "mutating tool %q leaked into research via buildVariantsServer", name)
	}
}

func TestNewRejectsInvalidVariant(t *testing.T) {
	cfg := &tgclient.Config{APIID: 1, APIHash: "x"}
	_, err := New(Options{Config: cfg, Variant: "bogus"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown variant")
	// The error must enumerate the accepted values (derived from variantDefs).
	assert.Contains(t, err.Error(), "full, compact, research")

	for _, v := range []string{"", "full", "compact", "research"} {
		_, err := New(Options{Config: cfg, Variant: v})
		require.NoErrorf(t, err, "variant %q should be accepted", v)
	}
}

func TestVariantCompactShortensDescriptions(t *testing.T) {
	full, _ := buildTestHandlers(t)
	impl := &mcp.Implementation{Name: "mcp-telegram", Version: "test"}

	fullNames := listToolNames(t, newInner(impl, nil, full, nil, false, testLogger()))
	compactNames := listToolNames(t, newInner(impl, nil, full, nil, true, testLogger()))

	require.Len(t, compactNames, len(fullNames), "compact keeps every tool, only trims descriptions")

	// GetChats has a multi-sentence description, so compact must be strictly shorter.
	require.Contains(t, fullNames, "GetChats")
	assert.Less(t, len(compactNames["GetChats"]), len(fullNames["GetChats"]),
		"compact GetChats description should be shortened")

	// In aggregate the compact variant must be meaningfully smaller.
	var fullTotal, compactTotal int
	for name, desc := range fullNames {
		fullTotal += len(desc)
		compactTotal += len(compactNames[name])
	}
	assert.Less(t, compactTotal, fullTotal, "compact descriptions must be smaller overall")
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

func TestValidVariant(t *testing.T) {
	for _, v := range []string{"", "full", "compact", "research"} {
		assert.Truef(t, validVariant(v), "%q should be valid", v)
	}
	for _, v := range []string{"Full", "monitoring", "read-only", "x"} {
		assert.Falsef(t, validVariant(v), "%q should be invalid", v)
	}
}

// TestVariantDefsTableInvariants guards the properties the rest of the code
// relies on but that live only in the table's element order/values: full must
// be first (priority 0, what non-negotiating clients silently receive), IDs
// must be unique (WithVariant panics otherwise, but we'd rather fail here), and
// each row's mode must match its ID. A reorder or typo here is a silent
// behavior change everywhere else, so pin it.
func TestVariantDefsTableInvariants(t *testing.T) {
	require.NotEmpty(t, variantDefs)
	assert.Equal(t, variantFull, variantDefs[0].meta.ID, "full must be at index 0 (priority 0, default for non-negotiating clients)")

	seen := map[string]bool{}
	for _, d := range variantDefs {
		assert.Falsef(t, seen[d.meta.ID], "duplicate variant ID %q in variantDefs", d.meta.ID)
		seen[d.meta.ID] = true
	}

	wantMode := map[string]serveMode{
		variantFull:     modeFull,
		variantCompact:  modeCompact,
		variantResearch: modeResearch,
	}
	for _, d := range variantDefs {
		assert.Equalf(t, wantMode[d.meta.ID], d.mode, "variant %q has unexpected serveMode", d.meta.ID)
	}
}

// TestServeModeFlags documents the mode → behavior mapping that newInner and
// buildVariantsServer branch on.
func TestServeModeFlags(t *testing.T) {
	assert.False(t, modeFull.compacts())
	assert.False(t, modeFull.researchOnly())
	assert.True(t, modeCompact.compacts())
	assert.False(t, modeCompact.researchOnly())
	assert.True(t, modeResearch.compacts(), "research is always compact by construction")
	assert.True(t, modeResearch.researchOnly())
}

// TestBuildVariantsServerPreservesFullDescriptions is the load-bearing safety
// test for handler sharing: full (inners[0], no compaction) and research
// (inners[2], compaction on) serve the *same* handler instances. If the compact
// middleware mutated the shared *mcp.Tool instead of a copy, listing the compact
// research variant would corrupt the full variant's descriptions. So we drive a
// tools/list through research first, then assert full is still full-length.
func TestBuildVariantsServerPreservesFullDescriptions(t *testing.T) {
	full, research := buildTestHandlers(t)
	impl := &mcp.Implementation{Name: "mcp-telegram", Version: "test"}

	vs, inners := buildVariantsServer(impl, nil, full, research, nil, testLogger())
	require.Len(t, inners, 3)
	require.NotNil(t, vs)

	// List the compact research variant first so any in-place mutation would
	// already have happened before we inspect the full variant.
	researchNames := listToolNames(t, inners[2])
	fullNames := listToolNames(t, inners[0])

	require.Contains(t, fullNames, "GetChats")
	require.Contains(t, researchNames, "GetChats")
	// research trimmed its copy; full must remain untouched and strictly longer.
	assert.Less(t, len(researchNames["GetChats"]), len(fullNames["GetChats"]),
		"research must trim its own description copy")
	assert.Greater(t, len(fullNames["GetChats"]), len(researchNames["GetChats"]),
		"full variant's shared handler description must survive a compact sibling's listing")
}

// TestResearchVariantShortensDescriptions verifies the research variant actually
// compacts (mode == modeResearch → compacts()). A regression that dropped
// compaction from research would still pass the count/leakage tests, so assert
// the description is shortened here.
func TestResearchVariantShortensDescriptions(t *testing.T) {
	full, research := buildTestHandlers(t)
	impl := &mcp.Implementation{Name: "mcp-telegram", Version: "test"}

	fullBaseline := listToolNames(t, newInner(impl, nil, full, nil, false, testLogger()))
	_, inners := buildVariantsServer(impl, nil, full, research, nil, testLogger())
	researchNames := listToolNames(t, inners[2])

	require.Contains(t, researchNames, "GetChats")
	require.Contains(t, fullBaseline, "GetChats")
	assert.Less(t, len(researchNames["GetChats"]), len(fullBaseline["GetChats"]),
		"research GetChats description should be shortened")
}

// TestRankedVariantsStableUnderHints pins the actual server-side ranking
// semantics: the default ranking sorts by priority (then status) and ignores
// client hints entirely. Hints are advisory metadata a client reads to pick a
// variant by ID — they do not reorder the server's recommendation. So even a
// client shouting "compact context" still gets full ranked first unless it
// explicitly selects another variant.
func TestRankedVariantsStableUnderHints(t *testing.T) {
	full, research := buildTestHandlers(t)
	impl := &mcp.Implementation{Name: "mcp-telegram", Version: "test"}
	vs, _ := buildVariantsServer(impl, nil, full, research, nil, testLogger())

	hints := variants.VariantHints{Hints: map[string]any{
		variants.HintContextSize: "compact",
		variants.HintUseCase:     "autonomous-agent",
	}}
	ranked := vs.RankedVariants(context.Background(), hints)
	require.Len(t, ranked, 3)
	assert.Equal(t, "full", ranked[0].ID, "default ranking ignores hints; full stays first")
	assert.Equal(t, "compact", ranked[1].ID)
	assert.Equal(t, "research", ranked[2].ID, "experimental ranks last")
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
	logger := testLogger()
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

// TestOverrideVariantSelection guards the single-variant override decision that
// runHappy makes (defForVariant → handler set + compaction). runHappy needs a
// live Telegram client, but the decision itself is pure, so we replay it here
// to keep it from drifting away from buildVariantsServer.
func TestOverrideVariantSelection(t *testing.T) {
	full, research := buildTestHandlers(t)
	impl := &mcp.Implementation{Name: "mcp-telegram", Version: "test"}
	fullBaseline := listToolNames(t, newInner(impl, nil, full, nil, false, testLogger()))

	cases := []struct {
		id          string
		wantTools   int
		wantCompact bool
	}{
		{variantFull, 29, false},
		{variantCompact, 29, true},
		{variantResearch, 16, true},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			d, ok := defForVariant(tc.id)
			require.True(t, ok)

			// Drive the exact constructor runHappy's override branch uses, so the
			// mode → (handlers, compaction) decision stays pinned to one place.
			srv := newInnerForMode(impl, nil, full, research, d.mode, nil, testLogger())
			names := listToolNames(t, srv)

			assert.Len(t, names, tc.wantTools)
			if tc.wantCompact {
				assert.Less(t, len(names["GetChats"]), len(fullBaseline["GetChats"]),
					"variant %q should serve compacted descriptions", tc.id)
			} else {
				assert.Equal(t, fullBaseline["GetChats"], names["GetChats"],
					"variant %q should serve full descriptions", tc.id)
			}
		})
	}
}

// TestCompactMiddlewarePassesThroughNilResult covers the branch that treats a
// successful tools/list with a typed-nil *mcp.ListToolsResult as a legitimate
// empty response: it must pass the nil through untouched (no panic, no
// reconstruction into an empty-but-non-nil result), distinct from the
// wrong-type Error branch above.
func TestCompactMiddlewarePassesThroughNilResult(t *testing.T) {
	logger := testLogger()
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

// listToolsThroughProxy drives tools/list through the real variants.Server proxy
// (not a direct inner-server connection), selecting a variant via the SEP-2053
// _meta key the way a negotiating client does. variantID == "" sends no
// selection, so the proxy serves its default (highest-priority) variant.
func listToolsThroughProxy(t *testing.T, vs *variants.Server, variantID string) map[string]string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	serverT, clientT := mcp.NewInMemoryTransports()
	runErr := make(chan error, 1)
	go func() { runErr <- vs.Run(ctx, serverT) }()
	t.Cleanup(func() {
		cancel()
		<-runErr
	})

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	require.NoError(t, err)
	defer func() { _ = cs.Close() }()

	out := map[string]string{}
	params := &mcp.ListToolsParams{}
	if variantID != "" {
		params.Meta = mcp.Meta{"io.modelcontextprotocol/server-variant": variantID}
	}
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

// TestVariantsProxyDispatchCompacts is the end-to-end guard the direct-connect
// tests can't give: it lists tools through the variants.Server proxy itself. The
// proxy captures each inner server's receiving chain at WithVariant time, so if
// someone reordered buildVariantsServer to call WithVariant before newInner
// installs compactToolsMiddleware, compaction would silently vanish for real
// proxied clients while every direct-connect test still passed. Selecting each
// variant by _meta and asserting the description lengths pins that invariant.
func TestVariantsProxyDispatchCompacts(t *testing.T) {
	full, research := buildTestHandlers(t)
	impl := &mcp.Implementation{Name: "mcp-telegram", Version: "test"}
	vs, _ := buildVariantsServer(impl, nil, full, research, nil, testLogger())

	fullNames := listToolsThroughProxy(t, vs, variantFull)
	compactNames := listToolsThroughProxy(t, vs, variantCompact)
	researchNames := listToolsThroughProxy(t, vs, variantResearch)
	defaultNames := listToolsThroughProxy(t, vs, "") // no selection → default variant

	assert.Len(t, fullNames, 29, "full via proxy exposes every tool")
	assert.Len(t, compactNames, 29, "compact via proxy keeps every tool")
	assert.Len(t, researchNames, 16, "research via proxy exposes the read-only subset")
	assert.Len(t, defaultNames, 29, "no selection must fall back to full (priority 0)")

	require.Contains(t, fullNames, "GetChats")
	// Compaction must actually reach the client through the proxy.
	assert.Less(t, len(compactNames["GetChats"]), len(fullNames["GetChats"]),
		"compact descriptions must be shortened when served through the proxy")
	assert.Less(t, len(researchNames["GetChats"]), len(fullNames["GetChats"]),
		"research descriptions must be shortened when served through the proxy")
	// The default (unselected) variant must be full, untrimmed.
	assert.Equal(t, fullNames["GetChats"], defaultNames["GetChats"],
		"default variant must serve full-length descriptions")
}
