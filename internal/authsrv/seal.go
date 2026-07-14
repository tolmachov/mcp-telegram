package authsrv

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// blobKind is the domain-separation tag of a sealed blob. It participates in
// the AEAD additional data together with the issuer, so a blob of one kind
// (or from another deployment) can never be opened as a different kind.
type blobKind string

const (
	kindState    blobKind = "state"
	kindCode     blobKind = "code"
	kindAccess   blobKind = "access"
	kindRefresh  blobKind = "refresh"
	kindClientID blobKind = "client_id"
)

// Public prefixes of the artifacts the server issues. They make token kinds
// recognizable in logs and bug reports without decryption.
const (
	prefixCode     = "mcp_ac_"
	prefixAccess   = "mcp_at_"
	prefixRefresh  = "mcp_rt_"
	prefixClientID = "mcp_cid_"
)

// Blob rejection reasons. errInvalidBlob is the umbrella every failure
// unwraps to — callers MUST NOT surface the specific reason to clients (no
// oracle), but SHOULD log it: the sub-reasons distinguish operational
// incidents (key rotated away, expired) from garbage or tampering.
var (
	errInvalidBlob  = errors.New("invalid sealed blob")
	errUnknownKeyID = fmt.Errorf("%w: sealed with a key not in the ring (rotated away or foreign deployment)", errInvalidBlob)
	errBlobExpired  = fmt.Errorf("%w: expired", errInvalidBlob)
)

// blobSpec fuses everything that must agree for one artifact type: the AAD
// kind, the public prefix, and (via the type parameter) the claims struct.
// Mispairing kind/prefix/claims is unrepresentable at call sites. A non-zero
// ttl makes openBlob enforce expiry itself, so "open then remember to check"
// cannot be forgotten; specs with ttl 0 carry their expiry inside the claims
// (access) or derive it from config (refresh) and are checked by the caller.
type blobSpec[T issuedAtCarrier] struct {
	kind   blobKind
	prefix string
	ttl    time.Duration
}

// The four sealed-blob specs. The DCR client_id is HMAC-signed, not sealed,
// and has its own dedicated functions.
var (
	stateBlob   = blobSpec[stateClaims]{kind: kindState, prefix: "", ttl: stateTTL}
	codeBlob    = blobSpec[codeClaims]{kind: kindCode, prefix: prefixCode, ttl: codeTTL}
	accessBlob  = blobSpec[accessClaims]{kind: kindAccess, prefix: prefixAccess}
	refreshBlob = blobSpec[refreshClaims]{kind: kindRefresh, prefix: prefixRefresh}
)

// sealer encrypts and signs the server's self-contained artifacts.
type sealer struct {
	ring   *keyRing
	issuer string
}

func newSealer(ring *keyRing, issuer string) *sealer {
	return &sealer{ring: ring, issuer: issuer}
}

// aad binds a blob to this deployment (issuer) and artifact kind.
func (s *sealer) aad(kind blobKind) []byte {
	return []byte(s.issuer + "|" + string(kind))
}

// sealBlob JSON-encodes v and returns prefix + base64url(keyID || nonce || ciphertext).
func sealBlob[T issuedAtCarrier](s *sealer, spec blobSpec[T], v T) (string, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encoding %s payload: %w", spec.kind, err)
	}
	k := s.ring.sealKey()
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generating nonce: %w", err)
	}
	buf := make([]byte, 0, 1+len(nonce)+len(payload)+k.aead.Overhead())
	buf = append(buf, k.id)
	buf = append(buf, nonce...)
	buf = k.aead.Seal(buf, nonce, payload, s.aad(spec.kind))
	return spec.prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// openBlob reverses sealBlob and enforces the spec's TTL. Every failure
// unwraps to errInvalidBlob; the concrete reason is for server-side logs only.
func openBlob[T issuedAtCarrier](s *sealer, spec blobSpec[T], blob string, now time.Time) (T, error) {
	var v T
	raw, ok := strings.CutPrefix(blob, spec.prefix)
	if !ok {
		return v, errInvalidBlob
	}
	buf, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(buf) == 0 {
		return v, errInvalidBlob
	}
	k, ok := s.ring.byID(buf[0])
	if !ok {
		return v, fmt.Errorf("%w (key id %#02x)", errUnknownKeyID, buf[0])
	}
	if len(buf) < 1+k.aead.NonceSize() {
		return v, errInvalidBlob
	}
	nonce := buf[1 : 1+k.aead.NonceSize()]
	payload, err := k.aead.Open(nil, nonce, buf[1+k.aead.NonceSize():], s.aad(spec.kind))
	if err != nil {
		return v, errInvalidBlob
	}
	if err := json.Unmarshal(payload, &v); err != nil {
		return v, errInvalidBlob
	}
	if spec.ttl > 0 && expired(v.issuedAt(), spec.ttl, now) {
		return v, errBlobExpired
	}
	return v, nil
}

// signClientID JSON-encodes v and returns mcp_cid_ + base64url(payload) +
// "." + base64url(HMAC). The DCR client_id is public (readable) but must be
// unforgeable.
func (s *sealer) signClientID(v clientIDClaims) (string, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encoding %s payload: %w", kindClientID, err)
	}
	k := s.ring.sealKey()
	mac := s.mac(k, kindClientID, payload)
	return prefixClientID + base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac), nil
}

// verifyClientID reverses signClientID, trying every ring key. Any failure
// yields errInvalidBlob.
func (s *sealer) verifyClientID(blob string, v *clientIDClaims) error {
	raw, ok := strings.CutPrefix(blob, prefixClientID)
	if !ok {
		return errInvalidBlob
	}
	payloadB64, macB64, ok := strings.Cut(raw, ".")
	if !ok {
		return errInvalidBlob
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return errInvalidBlob
	}
	mac, err := base64.RawURLEncoding.DecodeString(macB64)
	if err != nil {
		return errInvalidBlob
	}
	for _, k := range s.ring.keys {
		if hmac.Equal(mac, s.mac(k, kindClientID, payload)) {
			if err := json.Unmarshal(payload, v); err != nil {
				return errInvalidBlob
			}
			return nil
		}
	}
	return errInvalidBlob
}

// mac computes HMAC-SHA256 over the AAD and payload with the key's MAC subkey.
func (s *sealer) mac(k *ringKey, kind blobKind, payload []byte) []byte {
	h := hmac.New(sha256.New, k.mac)
	h.Write(s.aad(kind))
	h.Write([]byte{0})
	h.Write(payload)
	return h.Sum(nil)
}
