package tools

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// This file holds the shared opaque-cursor codec. Every paginating tool
// (GetMessages, SearchMessages, GetReplies, SearchMessagesGlobal, GetChats,
// GetForumTopics) encodes its resume state as a short JSON envelope tagged
// with a schema version and base64url-encodes it into a single opaque string
// through encodeCursor/decodeCursor, which checks the version (see
// versioned), so the wire format and exact-version validation rules are the
// same for all of them.

// encodeCursor renders a JSON-serialisable envelope as an opaque base64url
// string. RawURLEncoding (no padding) keeps the token count down since the
// encoded cursor is copied through tool I/O on every pagination hop. It panics
// if the envelope cannot be marshalled: every cursor envelope is a fixed struct
// of ints/strings whose Marshal cannot fail, and silently returning an empty
// cursor would mask that bug.
func encodeCursor[T any](env T) string {
	payload, err := json.Marshal(env)
	if err != nil {
		panic(fmt.Sprintf("marshalling cursor envelope: %v", err))
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
// tool: reject empty/whitespace input, base64url-decode, strictly decode JSON
// into the envelope T, then enforce the version policy against current.
// Unknown fields are rejected: a changed cursor shape requires a new version,
// and old/new cursor formats are never accepted by accident.
//
// Version policy is deliberately exact: cursor schemas are not migrated.
// Deployments that change a schema invalidate in-flight cursors and clients
// restart pagination without one.
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
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&env); err != nil {
		return env, fmt.Errorf("cursor payload is not valid JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return env, fmt.Errorf("cursor payload contains trailing JSON")
	}
	if got := env.cursorVersion(); got != current {
		return env, fmt.Errorf("cursor version %d is unsupported (expected %d); restart pagination without the cursor", got, current)
	}
	return env, nil
}
