package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/experimental-ext-variants/go/sdk/variants"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tools"
)

// Server variant IDs (SEP-2053). full is the default/highest-priority variant,
// so clients that don't understand variants transparently get it.
const (
	variantFull     = "full"
	variantCompact  = "compact"
	variantResearch = "research"
)

// JSON-RPC method names the middlewares key off. The SDK's own constants are
// unexported, so we spell them out.
const (
	methodListTools = "tools/list"
	methodCallTool  = "tools/call"
)

// serveMode says how a variant serves tools. Encoding it as one enum instead of
// two independent bools makes illegal combinations (e.g. "read-only subset but
// full-length descriptions") unrepresentable: every research variant is compact
// by construction, because modeResearch.compacts() is true.
type serveMode int

const (
	modeFull     serveMode = iota // every tool, full descriptions
	modeCompact                   // every tool, first-sentence descriptions
	modeResearch                  // read-only subset, first-sentence descriptions
)

// compacts reports whether this mode trims tool descriptions to the first
// sentence (installs compactToolsMiddleware on the inner server).
func (m serveMode) compacts() bool { return m == modeCompact || m == modeResearch }

// researchOnly reports whether this mode serves the read-only research subset
// instead of the full tool set.
func (m serveMode) researchOnly() bool { return m == modeResearch }

// variantDef is one row of the variant table: its SEP-2053 metadata plus the
// serveMode that says which tool set and description style it exposes.
type variantDef struct {
	meta variants.ServerVariant
	mode serveMode
}

// variantDefs is the single source of truth for the variant set, listed in
// priority order (index == priority passed to WithVariant). validVariant, the
// New error message, buildVariantsServer, and the --variant override path all
// derive from this table, so adding a variant means editing one place. full is
// first (priority 0), so clients that don't negotiate a variant receive it and
// behave exactly as before this feature existed.
var variantDefs = []variantDef{
	{
		meta: variants.ServerVariant{
			ID:          variantFull,
			Description: "All Telegram tools with complete descriptions. Best for interactive research and administration with a human in the loop.",
			Hints:       map[string]string{variants.HintUseCase: "human-assistant", variants.HintContextSize: "standard"},
			Status:      variants.Stable,
		},
		mode: modeFull,
	},
	{
		meta: variants.ServerVariant{
			ID:          variantCompact,
			Description: "All Telegram tools with concise (~50% shorter) descriptions. Best for autonomous agents and tight context budgets.",
			Hints:       map[string]string{variants.HintUseCase: "autonomous-agent", variants.HintContextSize: "compact"},
			Status:      variants.Stable,
		},
		mode: modeCompact,
	},
	{
		meta: variants.ServerVariant{
			ID:          variantResearch,
			Description: "Telegram read-only research subset: search, fetch, summarize, and export to a local file — no sending, editing, or admin. For read-heavy context-loading agents.",
			Hints:       map[string]string{variants.HintUseCase: "autonomous-agent", variants.HintContextSize: "compact"},
			Status:      variants.Experimental,
		},
		mode: modeResearch,
	},
}

// defForVariant returns the table row for a variant ID. ok is false for the
// empty string ("expose all") and for unknown IDs. The returned value shares its
// meta.Hints map with the package-global variantDefs row, so callers must treat
// it as read-only — mutating Hints would corrupt the table for every other
// lookup. No caller mutates it today.
func defForVariant(id string) (variantDef, bool) {
	for _, d := range variantDefs {
		if d.meta.ID == id {
			return d, true
		}
	}
	return variantDef{}, false
}

// variantIDs returns the known variant IDs in priority order, for error text.
func variantIDs() []string {
	ids := make([]string, len(variantDefs))
	for i, d := range variantDefs {
		ids[i] = d.meta.ID
	}
	return ids
}

// validVariant reports whether id names a known variant. The empty string is
// valid: it means "expose all variants" (no --variant override).
func validVariant(id string) bool {
	if id == "" {
		return true
	}
	_, ok := defForVariant(id)
	return ok
}

// compactDesc returns the first sentence of a tool description. The compact and
// research variants use it to roughly halve tools/list token cost.
//
// A period/question/exclamation mark ends the sentence only when it is at the
// end of the string, or is followed by whitespace and then a capital letter.
// Decimals ("3.14") and ellipses inside a URI ("telegram://media/...") are never
// followed by whitespace, so they can't truncate. Abbreviations ("e.g.", "i.e.")
// survive only because they are conventionally followed by a lowercase word — a
// capitalised word right after one would still be treated as a boundary. When no
// boundary exists the whole (already single-sentence) description is returned.
func compactDesc(full string) string {
	s := strings.TrimSpace(full)
	for i := 0; i < len(s); i++ {
		if s[i] != '.' && s[i] != '!' && s[i] != '?' {
			continue
		}
		tail := s[i+1:]
		trimmed := strings.TrimLeft(tail, " \t\r\n")
		if trimmed == "" {
			return s[:i+1] // terminator at end of string
		}
		// Require that we actually skipped whitespace and the next sentence
		// starts with a capital letter — otherwise it's an abbreviation/number.
		if len(trimmed) < len(tail) {
			if r, _ := utf8.DecodeRuneInString(trimmed); unicode.IsUpper(r) {
				return s[:i+1]
			}
		}
	}
	return s
}

// compactToolsMiddleware rewrites each tool's description to its first sentence
// in tools/list responses, leaving every other method untouched. It copies the
// result slice and each Tool before mutating, because the SDK's tools/list
// handler returns pointers straight to *this* server's stored *mcp.Tool values:
// mutating them in place would permanently shrink this server's own live
// registry and race concurrent tools/list calls. Each variant is a separate
// mcp.Server with its own Tool structs (every handler's Register builds a fresh
// *mcp.Tool per server), so the compact/research middleware never sees — and so
// cannot corrupt — the full variant's tools; the copy guards this server's own
// registry, not any cross-variant shared state.
// If the result is ever not a *mcp.ListToolsResult (a future SDK shape change),
// it logs at Error and passes the response through uncompacted rather than
// silently doing nothing: this condition nullifies the compact/research
// variants' entire reason to exist (tools/list token savings), so it must be
// loud enough to reach whatever alerts on Error, not buried among Warn noise.
func compactToolsMiddleware(logger *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil || method != methodListTools {
				return res, err
			}
			lt, ok := res.(*mcp.ListToolsResult)
			if !ok {
				logger.Error("compact variant: tools/list result was not *mcp.ListToolsResult; serving descriptions uncompacted",
					"type", fmt.Sprintf("%T", res))
				return res, err
			}
			if lt == nil {
				// A successful tools/list with a nil result is a legitimate
				// empty response — nothing to compact, pass it through.
				return res, err
			}
			out := *lt
			out.Tools = make([]*mcp.Tool, len(lt.Tools))
			for i, t := range lt.Tools {
				tc := *t
				tc.Description = compactDesc(tc.Description)
				out.Tools[i] = &tc
			}
			return &out, nil
		}
	}
}

// newInner builds one inner mcp.Server: it registers the given tool handlers,
// runs wire (resources/template/prompts), installs the request-logging
// middleware, and — when compact is true — the description-shortening one.
func newInner(impl *mcp.Implementation, opts *mcp.ServerOptions, handlers []tools.Handler, wire func(*mcp.Server), compact bool, logger *slog.Logger) *mcp.Server {
	s := mcp.NewServer(impl, opts)
	tools.RegisterTools(s, handlers)
	if wire != nil {
		wire(s)
	}
	s.AddReceivingMiddleware(requestLogMiddleware(logger))
	if compact {
		s.AddReceivingMiddleware(compactToolsMiddleware(logger))
	}
	return s
}

// newInnerForMode builds one inner server for a serveMode, deriving both the
// tool set (full vs the read-only research subset) and description compaction
// from that single value. Keeping the (handlers, compact) derivation here —
// rather than duplicated at each call site — is what makes the "research ⇒
// compact, research ⇒ read-only subset" invariant hold by construction: a
// caller cannot pair research handlers with full-length descriptions, because
// it never chooses the two independently.
func newInnerForMode(impl *mcp.Implementation, opts *mcp.ServerOptions, full, research []tools.Handler, mode serveMode, wire func(*mcp.Server), logger *slog.Logger) *mcp.Server {
	handlers := full
	if mode.researchOnly() {
		handlers = research
	}
	return newInner(impl, opts, handlers, wire, mode.compacts(), logger)
}

// buildVariantsServer wires every variant in variantDefs over shared Telegram
// deps and returns the variant-aware server plus the inner servers in priority
// order. The caller registers pinned-chat resources on all of them (so every
// variant can list/read them) while driving a single poller — running one poll
// per variant would multiply Telegram polling and risk FLOOD_WAIT.
func buildVariantsServer(impl *mcp.Implementation, opts *mcp.ServerOptions, full, research []tools.Handler, wire func(*mcp.Server), logger *slog.Logger) (*variants.Server, []*mcp.Server) {
	vs := variants.NewServer(impl)
	inners := make([]*mcp.Server, len(variantDefs))
	for i, d := range variantDefs {
		srv := newInnerForMode(impl, opts, full, research, d.mode, wire, logger)
		inners[i] = srv
		vs.WithVariant(d.meta, srv, i)
	}
	return vs, inners
}
