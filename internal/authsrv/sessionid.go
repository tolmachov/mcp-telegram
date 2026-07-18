package authsrv

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// sessionIDLen is the byte length of a raw session id before hex encoding. 16
// bytes (128 bits) is collision-safe across a deployment's sessions and yields
// a 32-char hex string safe for use in a bucket object name / file path.
const sessionIDLen = 16

// sessionKeyLen is the byte length of the per-session encryption key mixed into
// the session AEAD. It matches the 32-byte AES-256 key size.
const sessionKeyLen = 32

// newSessionCreds mints a fresh independent-session identity: a random session
// id (the object-name suffix, not secret) and a random per-session encryption
// key (secret, carried only inside the OAuth token). They are deliberately
// distinct values — the sid is public in the bucket listing and must never be
// the key.
func newSessionCreds() (sid string, key []byte, err error) {
	raw := make([]byte, sessionIDLen)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("generating session id: %w", err)
	}
	key = make([]byte, sessionKeyLen)
	if _, err := rand.Read(key); err != nil {
		return "", nil, fmt.Errorf("generating session key: %w", err)
	}
	return hex.EncodeToString(raw), key, nil
}

// validSessionID reports whether s is a well-formed session id: lowercase hex
// of exactly sessionIDLen bytes. Session ids reach the storage layer as an
// object-name/path suffix, so validating the format (server-minted, never
// user-supplied) keeps a malformed value from ever influencing a path.
func validSessionID(s string) bool {
	if len(s) != hex.EncodedLen(sessionIDLen) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
