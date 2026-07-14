package authsrv

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// Domain-separation labels for the HKDF subkeys derived from each master key.
const (
	hkdfInfoAEAD = "mcp-telegram/authsrv/aead/v1"
	hkdfInfoMAC  = "mcp-telegram/authsrv/mac/v1"
)

const masterKeyLen = 32

// ringKey is one master key expanded into its usable subkeys. The one-byte id
// (first byte of the master key's SHA-256) is stored in every sealed blob so
// the ring can pick the right key during rotation.
type ringKey struct {
	id   byte
	aead cipher.AEAD
	mac  []byte
}

// keyRing holds all accepted master keys. keys[0] seals and signs new blobs;
// every key can open and verify existing ones.
type keyRing struct {
	keys []*ringKey
}

// newKeyRing parses base64-encoded 32-byte master keys into a key ring.
func newKeyRing(encoded []string) (*keyRing, error) {
	if len(encoded) == 0 {
		return nil, fmt.Errorf("no keys provided")
	}
	ring := &keyRing{}
	seen := map[byte]int{}
	for i, e := range encoded {
		master, err := decodeMasterKey(e)
		if err != nil {
			return nil, fmt.Errorf("key %d: %w", i, err)
		}
		k, err := deriveRingKey(master)
		if err != nil {
			return nil, fmt.Errorf("key %d: %w", i, err)
		}
		if prev, dup := seen[k.id]; dup {
			return nil, fmt.Errorf("keys %d and %d collide on key ID %d: replace one of them", prev, i, k.id)
		}
		seen[k.id] = i
		ring.keys = append(ring.keys, k)
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
			if len(b) != masterKeyLen {
				return nil, fmt.Errorf("decoded key is %d bytes, want %d", len(b), masterKeyLen)
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("key is not valid base64")
}

// deriveRingKey expands a master key into AEAD and MAC subkeys via HKDF.
func deriveRingKey(master []byte) (*ringKey, error) {
	sum := sha256.Sum256(master)

	aeadKey, err := hkdf.Key(sha256.New, master, nil, hkdfInfoAEAD, 32)
	if err != nil {
		return nil, fmt.Errorf("deriving AEAD key: %w", err)
	}
	block, err := aes.NewCipher(aeadKey)
	if err != nil {
		return nil, fmt.Errorf("creating AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	macKey, err := hkdf.Key(sha256.New, master, nil, hkdfInfoMAC, 32)
	if err != nil {
		return nil, fmt.Errorf("deriving MAC key: %w", err)
	}

	return &ringKey{id: sum[0], aead: aead, mac: macKey}, nil
}

// sealKey returns the key used for new blobs and signatures.
func (r *keyRing) sealKey() *ringKey { return r.keys[0] }

// byID returns the key with the given id, if present.
func (r *keyRing) byID(id byte) (*ringKey, bool) {
	for _, k := range r.keys {
		if k.id == id {
			return k, true
		}
	}
	return nil, false
}
