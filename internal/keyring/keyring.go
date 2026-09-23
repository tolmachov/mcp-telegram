// Package keyring parses the MCP_AUTH_TOKEN_KEYS master keys shared by the
// OAuth token sealer (authsrv) and the session cipher (sessionstore). It only
// decodes and identifies keys; each consumer derives its own subkeys with its
// own HKDF labels, so neither can open the other's blobs.
package keyring

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// MasterKeyLen is the required length of a decoded master key.
const MasterKeyLen = 32

// Key is one master key. ID (the first byte of the key's SHA-256) is stored
// in every blob sealed under it so the ring can pick the right key during
// rotation.
type Key struct {
	ID     byte
	Master []byte
}

// Ring holds all accepted master keys. The first key seals new blobs; every
// key opens existing ones.
type Ring struct {
	keys []Key
}

// Parse decodes base64-encoded 32-byte master keys into a ring. Two keys
// sharing an ID are rejected: the ID is what selects the key on open.
func Parse(encoded []string) (*Ring, error) {
	if len(encoded) == 0 {
		return nil, errors.New("no keys provided")
	}
	ring := &Ring{}
	seen := map[byte]int{}
	for i, e := range encoded {
		master, err := decodeMasterKey(e)
		if err != nil {
			return nil, fmt.Errorf("key %d: %w", i, err)
		}
		sum := sha256.Sum256(master)
		id := sum[0]
		if prev, dup := seen[id]; dup {
			return nil, fmt.Errorf("keys %d and %d collide on key ID %d: replace one of them", prev, i, id)
		}
		seen[id] = i
		ring.keys = append(ring.keys, Key{ID: id, Master: master})
	}
	return ring, nil
}

// decodeMasterKey accepts standard or URL-safe base64, padded or raw.
func decodeMasterKey(e string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(e); err == nil {
			if len(b) != MasterKeyLen {
				return nil, fmt.Errorf("decoded key is %d bytes, want %d", len(b), MasterKeyLen)
			}
			return b, nil
		}
	}
	return nil, errors.New("key is not valid base64")
}

// Keys returns every key, the sealing key first.
func (r *Ring) Keys() []Key { return r.keys }

// Primary returns the key that seals new blobs.
func (r *Ring) Primary() Key { return r.keys[0] }

// ByID returns the key with the given ID, if present.
func (r *Ring) ByID(id byte) (Key, bool) {
	for _, k := range r.keys {
		if k.ID == id {
			return k, true
		}
	}
	return Key{}, false
}

// NewGCM builds an AES-256-GCM AEAD from a 32-byte derived key.
func NewGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}
	return aead, nil
}
