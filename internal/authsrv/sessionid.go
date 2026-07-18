package authsrv

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
)

// sessionIDLen is the byte length of a raw session id before hex encoding. 16
// bytes (128 bits) is collision-safe across a deployment's sessions and yields
// a 32-char hex string safe for use in a bucket object name / file path.
const sessionIDLen = 16

// sessionKeyLen is the byte length of the per-session encryption key mixed into
// the session AEAD. It matches the 32-byte AES-256 key size.
const sessionKeyLen = 32

// randomHex returns nBytes of cryptographic randomness as a lowercase hex
// string. It is the single minting path for the server's random hex ids
// (login ids and session ids), so their format cannot drift apart.
func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// newSessionCreds mints a fresh independent-session identity: a random session
// id (the object-name suffix, not secret) and a random per-session encryption
// key (secret, carried only inside the OAuth token). They are deliberately
// distinct values — the sid is public in the bucket listing and must never be
// the key.
func newSessionCreds() (sid string, key []byte, err error) {
	sid, err = randomHex(sessionIDLen)
	if err != nil {
		return "", nil, err
	}
	key = make([]byte, sessionKeyLen)
	if _, err := rand.Read(key); err != nil {
		return "", nil, fmt.Errorf("generating session key: %w", err)
	}
	return sid, key, nil
}

// validSessionID reports whether s is a well-formed session id. It is the trust
// boundary for sids that arrive inside a token blob (revocation, refresh):
// server-minted sids always pass, and a malformed value is rejected before it
// can reach the storage layer as an object-name / path suffix. Delegates to
// sessionstore.ValidSID so mint and check share one definition.
func validSessionID(s string) bool {
	return sessionstore.ValidSID(s)
}
