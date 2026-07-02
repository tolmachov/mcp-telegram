package tools

import (
	"github.com/tolmachov/mcp-telegram/internal/messages"
)

// cursorEnvelope is the wire form of a GlobalSearchCursor — a small JSON
// object base64(url)-encoded into a single opaque string. Field names are
// kept short because the full encoded cursor ends up in tool outputs and
// inputs, and LLMs count against context on every copy.
//
// The envelope carries a version tag so the decoder can distinguish an
// old cursor it can still understand from a newer one it cannot. Bumping
// cursorVersion and teaching ParseGlobalSearchCursor to migrate earlier
// shapes is how future schema changes stay backwards-compatible without
// invalidating every in-flight pagination.
type cursorEnvelope struct {
	Version int               `json:"v"`
	Rate    int               `json:"r"`
	Kind    messages.PeerKind `json:"k"`
	ID      int64             `json:"i"`
	Hash    int64             `json:"h,omitempty"`
	Msg     int               `json:"m"`
}

func (e cursorEnvelope) cursorVersion() int { return e.Version }

// cursorSchemaVersion is the current envelope schema version.
const cursorSchemaVersion = 1

// FormatGlobalSearchCursor renders a cursor as an opaque base64 string.
// Invariant: ParseGlobalSearchCursor(FormatGlobalSearchCursor(c)) round-trips
// exactly for every cursor returned by the provider.
func FormatGlobalSearchCursor(c messages.GlobalSearchCursor) string {
	env := cursorEnvelope{
		Version: cursorSchemaVersion,
		Rate:    c.Rate,
		Kind:    c.PeerKind,
		ID:      c.PeerID,
		Hash:    c.AccessHash,
		Msg:     c.MsgID,
	}
	return encodeCursor(env)
}

// ParseGlobalSearchCursor decodes an opaque cursor string produced by a
// prior SearchGlobal call. Structural validation (base64, JSON, version) is
// enforced here; field-level invariants (known peer kind, positive IDs,
// access_hash presence) are enforced by messages.NewGlobalSearchCursor.
// Together they ensure the LLM gets a descriptive error instead of a silent
// bad-pagination loop.
func ParseGlobalSearchCursor(s string) (*messages.GlobalSearchCursor, error) {
	env, err := decodeCursor[cursorEnvelope](s, cursorSchemaVersion)
	if err != nil {
		return nil, err
	}

	// Delegate validation and construction to NewGlobalSearchCursor so the
	// invariant rules live in a single place; both the wire-parser and
	// direct-construction paths are always in sync.
	return messages.NewGlobalSearchCursor(env.Rate, env.Kind, env.ID, env.Hash, env.Msg)
}
