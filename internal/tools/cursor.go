package tools

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// This file holds the shared opaque-cursor codec. Every paginating tool
// (SearchMessagesGlobal, GetChats, GetForumTopics) encodes its resume state as
// a short JSON envelope, base64url-encodes it into a single opaque string, and
// tags it with a schema version. The three used to carry hand-copied variants
// of this logic that had already drifted apart (notably their version policy);
// they now share encodeCursor/decodeCursor — with the version check folded into
// decodeCursor via the versioned interface — so the wire format and
// compatibility rules stay identical.

// encodeCursor renders a JSON-serialisable envelope as an opaque base64url
// string. RawURLEncoding (no padding) keeps the token count down since the
// encoded cursor is copied through tool I/O on every pagination hop. It panics
// if the envelope cannot be marshalled: every cursor envelope is a fixed struct
// of ints/strings whose Marshal cannot fail, and silently returning an empty
// cursor would mask that bug.
func encodeCursor[T any](env T) string {
	payload, err := json.Marshal(env)
	if err != nil {
		panic(fmt.Sprintf("marshaling cursor envelope: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

// versioned is implemented by every cursor envelope so decodeCursor can read
// and enforce the schema version itself. Folding the check into decodeCursor
// (rather than leaving it a separate call the caller must remember) makes it
// impossible to decode a cursor without validating its version.
type versioned interface {
	cursorVersion() int
}

// decodeCursor performs the structural half of cursor parsing shared by every
// tool: reject empty/whitespace input, base64url-decode, JSON-unmarshal into
// the envelope T, then enforce the version policy against current. Field-level
// validation (offsets, peer kinds) stays with the caller. DisallowUnknownFields
// is deliberately NOT used — the version tag, not a strict decoder, is the
// forward-compat mechanism, so a future cursor that adds an optional field
// still decodes here and is then accepted/rejected purely on its version.
//
// Version policy: a cursor at or below current is accepted (version 0 means a
// pre-versioning cursor and is tolerated); a higher version comes from a newer
// build we cannot interpret, and the returned error tells the LLM to restart
// pagination rather than loop on an offset it can't decode.
func decodeCursor[T versioned](s string, current int) (T, error) {
	var env T
	if s == "" {
		return env, fmt.Errorf("cursor is empty")
	}
	if s != strings.TrimSpace(s) {
		return env, fmt.Errorf("cursor contains whitespace")
	}

	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return env, fmt.Errorf("cursor is not valid base64url: %w", err)
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, fmt.Errorf("cursor payload is not valid JSON: %w", err)
	}
	if got := env.cursorVersion(); got > current {
		return env, fmt.Errorf("cursor version %d is newer than supported (%d); restart pagination without the cursor", got, current)
	}
	return env, nil
}
