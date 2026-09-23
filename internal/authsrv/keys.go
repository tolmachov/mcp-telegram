package authsrv

import (
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"

	"github.com/tolmachov/mcp-telegram/internal/keyring"
)

// Domain-separation labels for the HKDF subkeys derived from each master key.
const (
	hkdfInfoAEAD = "mcp-telegram/authsrv/aead/v1"
	hkdfInfoMAC  = "mcp-telegram/authsrv/mac/v1"
)

// ringKey is one master key expanded into its usable subkeys. The one-byte id
// (see keyring.Key) is stored in every sealed blob so the ring can pick the
// right key during rotation.
type ringKey struct {
	id   byte
	aead cipher.AEAD
	mac  []byte
}

// keyRing holds the subkeys of every accepted master key. keys[0] seals and
// signs new blobs; every key can open and verify existing ones.
type keyRing struct {
	keys []*ringKey
}

// newKeyRing derives the token subkeys of every master key in ring.
func newKeyRing(ring *keyring.Ring) (*keyRing, error) {
	r := &keyRing{}
	for i, k := range ring.Keys() {
		rk, err := deriveRingKey(k)
		if err != nil {
			return nil, fmt.Errorf("key %d: %w", i, err)
		}
		r.keys = append(r.keys, rk)
	}
	return r, nil
}

// deriveRingKey expands a master key into AEAD and MAC subkeys via HKDF.
func deriveRingKey(k keyring.Key) (*ringKey, error) {
	aeadKey, err := hkdf.Key(sha256.New, k.Master, nil, hkdfInfoAEAD, 32)
	if err != nil {
		return nil, fmt.Errorf("deriving AEAD key: %w", err)
	}
	aead, err := keyring.NewGCM(aeadKey)
	if err != nil {
		return nil, fmt.Errorf("token AEAD: %w", err)
	}
	macKey, err := hkdf.Key(sha256.New, k.Master, nil, hkdfInfoMAC, 32)
	if err != nil {
		return nil, fmt.Errorf("deriving MAC key: %w", err)
	}
	return &ringKey{id: k.ID, aead: aead, mac: macKey}, nil
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
