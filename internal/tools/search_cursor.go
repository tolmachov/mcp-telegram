package tools

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

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
	Version int    `json:"v"`
	Rate    int    `json:"r"`
	Kind    messages.PeerKind `json:"k"`
	ID      int64  `json:"i"`
	Hash    int64  `json:"h,omitempty"`
	Msg     int    `json:"m"`
}

// cursorVersion is the current envelope schema version.
const cursorVersion = 1

// FormatGlobalSearchCursor renders a cursor as an opaque base64 string.
// Invariant: ParseGlobalSearchCursor(FormatGlobalSearchCursor(c)) round-trips
// exactly for every cursor returned by the provider.
func FormatGlobalSearchCursor(c messages.GlobalSearchCursor) string {
	env := cursorEnvelope{
		Version: cursorVersion,
		Rate:    c.Rate,
		Kind:    c.PeerKind,
		ID:      c.PeerID,
		Hash:    c.AccessHash,
		Msg:     c.MsgID,
	}
	payload, err := json.Marshal(env)
	if err != nil {
		// cursorEnvelope is a fixed struct of ints and strings; json.Marshal
		// of it cannot fail. If this ever triggers, something is very wrong
		// and silently returning an empty cursor would mask the bug.
		panic(fmt.Sprintf("marshaling cursor envelope: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

// ParseGlobalSearchCursor decodes an opaque cursor string produced by a
// prior SearchGlobal call. Structural validation (base64, JSON, version) is
// enforced here; field-level invariants (known peer kind, positive IDs,
// access_hash presence) are enforced by messages.NewGlobalSearchCursor.
// Together they ensure the LLM gets a descriptive error instead of a silent
// bad-pagination loop.
func ParseGlobalSearchCursor(s string) (*messages.GlobalSearchCursor, error) {
	if s == "" {
		return nil, fmt.Errorf("cursor is empty")
	}
	if s != strings.TrimSpace(s) {
		return nil, fmt.Errorf("cursor contains whitespace")
	}

	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("cursor is not valid base64url: %w", err)
	}

	// We intentionally do NOT call dec.DisallowUnknownFields(): the Version
	// field IS the schema-compat mechanism. Forward-compat is broken if the
	// decoder rejects any unknown key before we get a chance to check the
	// version — a future v2 cursor with a new field would otherwise fail
	// with a generic JSON error instead of the helpful "newer than
	// supported" hint below.
	var env cursorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("cursor payload is not valid JSON: %w", err)
	}

	// Accept cursors at or below the current version. Future schema changes
	// bump cursorVersion; older cursors remain parseable until an explicit
	// migration removes them. Cursors claiming a higher version than we
	// know about come from a newer server we cannot interpret — tell the
	// caller to restart pagination rather than guessing.
	if env.Version > cursorVersion {
		return nil, fmt.Errorf("cursor version %d is newer than supported (%d); restart pagination without the offset_cursor", env.Version, cursorVersion)
	}

	// Delegate validation and construction to NewGlobalSearchCursor so the
	// invariant rules live in a single place; both the wire-parser and
	// direct-construction paths are always in sync.
	return messages.NewGlobalSearchCursor(env.Rate, env.Kind, env.ID, env.Hash, env.Msg)
}
