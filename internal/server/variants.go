package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tools"
)

// Server variant IDs, chosen at startup with --variant. full is the default:
// the empty ID names it.
const (
	variantFull     = "full"
	variantCompact  = "compact"
	variantResearch = "research"
)

// JSON-RPC method names the middlewares key off. The SDK's own constants are
// unexported, so we spell them out.
const (
	methodListTools    = "tools/list"
	methodCallTool     = "tools/call"
	methodReadResource = "resources/read"
)

// serveMode says how a variant serves tools. Encoding it as one enum instead of
// two independent bools makes illegal combinations (e.g. "read-only subset but
// full-length descriptions") unrepresentable: research is compact by
// construction, because modeResearch.compacts() is true.
type serveMode int

const (
	modeFull     serveMode = iota // every tool, full descriptions
	modeCompact                   // every tool, first-sentence descriptions
	modeResearch                  // read-only subset, first-sentence descriptions
)

// compacts reports whether this mode trims tool descriptions to the first
// sentence (installs compactToolsMiddleware on the server).
func (m serveMode) compacts() bool { return m == modeCompact || m == modeResearch }

// researchOnly reports whether this mode serves the read-only research subset
// instead of the full tool set.
func (m serveMode) researchOnly() bool { return m == modeResearch }

// variantDef is one row of the variant table: a variant ID and the serveMode
// it names.
type variantDef struct {
	id   string
	mode serveMode
}

// variantDefs is the single source of truth for the variant set. New's
// validation, its error message and buildAssembly all derive from it, so adding
// a variant means editing one place.
var variantDefs = []variantDef{
	{id: variantFull, mode: modeFull},
	{id: variantCompact, mode: modeCompact},
	{id: variantResearch, mode: modeResearch},
}

// modeForVariant returns the serveMode a variant ID names; ok is false for an
// unknown ID. The empty ID names full.
func modeForVariant(id string) (mode serveMode, ok bool) {
	if id == "" {
		id = variantFull
	}
	for _, d := range variantDefs {
		if d.id == id {
			return d.mode, true
		}
	}
	return 0, false
}

// variantIDs returns the known variant IDs, for error text.
func variantIDs() []string {
	ids := make([]string, len(variantDefs))
	for i, d := range variantDefs {
		ids[i] = d.id
	}
	return ids
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
// mutating them in place would permanently shrink the server's live registry
// and race concurrent tools/list calls.
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

// newServerOptions returns the options every MCP server here starts from. The
// capabilities are empty rather than nil: the SDK's nil default advertises the
// logging capability, which MCP deprecated (SEP-2577) and this server does not
// use; the SDK adds the tools, resources, prompts and completions capabilities
// from what is registered.
func newServerOptions(instructions string, logger *slog.Logger) *mcp.ServerOptions {
	return &mcp.ServerOptions{
		Instructions: instructions,
		Logger:       logger,
		Capabilities: &mcp.ServerCapabilities{},
	}
}

// newModeServer builds the MCP server for the server's mode: it registers
// handlers (the set buildHandlers made for that mode), runs wire (resources,
// chat template, prompts and the client-down middleware, which must sit
// inside the request log), installs the request-logging middleware and, when
// the mode compacts, the description-shortening one.
func (s *Server) newModeServer(impl *mcp.Implementation, opts *mcp.ServerOptions, handlers []tools.Handler, wire func(*mcp.Server), logger *slog.Logger) *mcp.Server {
	srv := mcp.NewServer(impl, opts)
	tools.RegisterTools(srv, handlers)
	wire(srv)
	srv.AddReceivingMiddleware(requestLogMiddleware(logger))
	if s.mode.compacts() {
		srv.AddReceivingMiddleware(compactToolsMiddleware(logger))
	}
	return srv
}
