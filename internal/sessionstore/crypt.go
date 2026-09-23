package sessionstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// hkdfInfoSession domain-separates the session-encryption subkey from the
// authsrv token subkeys derived from the same MCP_AUTH_TOKEN_KEYS master keys: a
// component that can decrypt sessions must not be able to mint tokens and
// vice versa. The v3 key additionally folds in the per-authorization userKey
// via the HKDF salt.
const hkdfInfoSession = "mcp-telegram/sessionstore/aead/v3"

// sessionBlobVersion is the mandatory first byte of every supported blob.
const sessionBlobVersion = 0x03

const masterKeyLen = 32

// userKeyLen is the required length of a v3 per-session key (matches the key
// authsrv mints). Enforced in aeadFor so a short/low-entropy key can never seal
// or open a v3 blob — the split-key protection must not silently degrade if an
// upstream bug ever passes a malformed key.
const userKeyLen = 32

// ErrCorruptSession is returned by LoadSession when a stored blob exists but
// cannot be decrypted (a rotated-away key, an issuer/AAD mismatch, a
// truncated object, or tampering). It is deliberately distinct from
// session.ErrNotFound: a present-but-unreadable session is an operator error
// (usually key management), not a "log in fresh" signal, so callers must NOT
// treat it as an empty store and must NOT delete the blob on it — that would
// turn a recoverable key mistake into permanent data loss.
var ErrCorruptSession = errors.New("sessionstore: cannot decrypt session blob")

// cryptKey is one master key. The one-byte id (first byte of the master key's
// SHA-256) prefixes every blob so decryption can pick the right key during
// rotation — same scheme as the authsrv key ring. AEADs are derived per
// (master, userKey) on demand, so master is retained for that derivation.
type cryptKey struct {
	id     byte
	master []byte
}

// Cipher encrypts session blobs with the first key and decrypts with any.
type Cipher struct {
	keys   []*cryptKey
	issuer string
}

// NewCipher parses base64-encoded 32-byte master keys (the MCP_AUTH_TOKEN_KEYS
// values) into a session cipher. The first key encrypts new blobs; all keys
// decrypt, enabling rotation. issuer participates in the AAD so blobs cannot
// travel between deployments.
func NewCipher(encodedKeys []string, issuer string) (*Cipher, error) {
	if len(encodedKeys) == 0 {
		return nil, fmt.Errorf("sessionstore: no keys provided")
	}
	c := &Cipher{issuer: issuer}
	seen := map[byte]int{}
	for i, e := range encodedKeys {
		master, err := decodeMasterKey(e)
		if err != nil {
			return nil, fmt.Errorf("sessionstore key %d: %w", i, err)
		}
		k := deriveCryptKey(master)
		if prev, dup := seen[k.id]; dup {
			return nil, fmt.Errorf("sessionstore keys %d and %d collide on key ID %d: replace one of them", prev, i, k.id)
		}
		seen[k.id] = i
		c.keys = append(c.keys, k)
	}
	return c, nil
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

func deriveCryptKey(master []byte) *cryptKey {
	sum := sha256.Sum256(master)
	m := make([]byte, len(master))
	copy(m, master)
	return &cryptKey{id: sum[0], master: m}
}

// newGCM builds an AES-256-GCM AEAD from a 32-byte key.
func newGCM(key []byte) (cipher.AEAD, error) {
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

// aeadFor returns the AEAD derived from both the master and per-session key.
func (c *Cipher) aeadFor(k *cryptKey, userKey []byte) (cipher.AEAD, error) {
	if len(userKey) != userKeyLen {
		return nil, fmt.Errorf("sessionstore: session key must be %d bytes, got %d", userKeyLen, len(userKey))
	}
	aeadKey, err := hkdf.Key(sha256.New, k.master, userKey, hkdfInfoSession, 32)
	if err != nil {
		return nil, fmt.Errorf("deriving v3 AEAD key: %w", err)
	}
	return newGCM(aeadKey)
}

// aad binds a blob to this deployment, format, and user.
func (c *Cipher) aad(userID tgid.UserID) []byte {
	return []byte(c.issuer + "|session|v3|" + userID.String())
}

// seal returns version || keyID || nonce || AEAD ciphertext.
func (c *Cipher) seal(userID tgid.UserID, userKey, plaintext []byte) ([]byte, error) {
	k := c.keys[0]
	aead, err := c.aeadFor(k, userKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}
	buf := make([]byte, 0, 2+len(nonce)+len(plaintext)+aead.Overhead())
	buf = append(buf, sessionBlobVersion)
	buf = append(buf, k.id)
	buf = append(buf, nonce...)
	return aead.Seal(buf, nonce, plaintext, c.aad(userID)), nil
}

// open reverses seal. Unsupported formats are rejected without migration.
func (c *Cipher) open(userID tgid.UserID, userKey, blob []byte) ([]byte, error) {
	if len(blob) < 2 || blob[0] != sessionBlobVersion {
		return nil, fmt.Errorf("%w: unsupported session blob version", ErrCorruptSession)
	}
	keyID := blob[1]
	var key *cryptKey
	for _, k := range c.keys {
		if k.id == keyID {
			key = k
			break
		}
	}
	if key == nil {
		return nil, fmt.Errorf("%w: sealed with a key not in the ring", ErrCorruptSession)
	}
	aead, err := c.aeadFor(key, userKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptSession, err)
	}
	headerLen := 2 + aead.NonceSize()
	if len(blob) < headerLen {
		return nil, ErrCorruptSession
	}
	nonce := blob[2:headerLen]
	plaintext, err := aead.Open(nil, nonce, blob[headerLen:], c.aad(userID))
	if err != nil {
		return nil, ErrCorruptSession
	}
	return plaintext, nil
}

// Encrypted wraps a backend so that it only ever sees AEAD ciphertext.
func Encrypted(inner Store, cipher *Cipher) Store {
	return &encryptedStore{inner: inner, cipher: cipher}
}

type encryptedStore struct {
	inner  Store
	cipher *Cipher
}

// validStoreSID guards the store boundary before a token-derived value can
// become an object-name or file-path suffix.
func validStoreSID(sid string) bool { return ValidSID(sid) }

// ErrInvalidSID is returned by every Store method when a caller presents a
// malformed non-empty sid (see validStoreSID).
var ErrInvalidSID = errors.New("sessionstore: invalid session id")

// brokenSession is returned by Session for an invalid sid; every operation
// fails with the same error so the mismatch surfaces immediately instead of
// building a path from unvalidated input.
type brokenSession struct{ err error }

func (b brokenSession) LoadSession(context.Context) ([]byte, error) { return nil, b.err }
func (b brokenSession) StoreSession(context.Context, []byte) error  { return b.err }

func (s *encryptedStore) Session(userID tgid.UserID, sid string, userKey []byte) session.Storage {
	if !validStoreSID(sid) {
		return brokenSession{err: ErrInvalidSID}
	}
	return &encryptedSession{
		inner:   s.inner.Session(userID, sid, nil),
		cipher:  s.cipher,
		userID:  userID,
		sid:     sid,
		userKey: userKey,
	}
}

func (s *encryptedStore) Exists(ctx context.Context, userID tgid.UserID, sid string) (bool, error) {
	if !validStoreSID(sid) {
		return false, ErrInvalidSID
	}
	ok, err := s.inner.Exists(ctx, userID, sid)
	if err != nil {
		return false, fmt.Errorf("encrypted store: %w", err)
	}
	return ok, nil
}

func (s *encryptedStore) Delete(ctx context.Context, userID tgid.UserID, sid string) error {
	if !validStoreSID(sid) {
		return ErrInvalidSID
	}
	if err := s.inner.Delete(ctx, userID, sid); err != nil {
		return fmt.Errorf("encrypted store: %w", err)
	}
	return nil
}

func (s *encryptedStore) List(ctx context.Context) ([]SessionRef, error) {
	refs, err := s.inner.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("encrypted store: %w", err)
	}
	return refs, nil
}

// Revocation tombstones carry no secret (mere presence is the signal), so the
// Encrypted wrapper delegates the tombstone methods straight through.

func (s *encryptedStore) Revoke(ctx context.Context, userID tgid.UserID, sid string) error {
	if !validStoreSID(sid) {
		return ErrInvalidSID
	}
	if err := s.inner.Revoke(ctx, userID, sid); err != nil {
		return fmt.Errorf("encrypted store: %w", err)
	}
	return nil
}

func (s *encryptedStore) Revoked(ctx context.Context, userID tgid.UserID, sid string) (bool, error) {
	if !validStoreSID(sid) {
		return false, ErrInvalidSID
	}
	ok, err := s.inner.Revoked(ctx, userID, sid)
	if err != nil {
		return false, fmt.Errorf("encrypted store: %w", err)
	}
	return ok, nil
}

func (s *encryptedStore) ListRevoked(ctx context.Context) ([]SessionRef, error) {
	refs, err := s.inner.ListRevoked(ctx)
	if err != nil {
		return nil, fmt.Errorf("encrypted store: %w", err)
	}
	return refs, nil
}

func (s *encryptedStore) DeleteRevoked(ctx context.Context, userID tgid.UserID, sid string) error {
	if !validStoreSID(sid) {
		return ErrInvalidSID
	}
	if err := s.inner.DeleteRevoked(ctx, userID, sid); err != nil {
		return fmt.Errorf("encrypted store: %w", err)
	}
	return nil
}

func (s *encryptedStore) RedeemCode(ctx context.Context, family, sid string, expiresAt time.Time) (bool, error) {
	return s.inner.RedeemCode(ctx, family, sid, expiresAt)
}

func (s *encryptedStore) RotateGrant(ctx context.Context, family string, generation int64) (GrantRotation, error) {
	return s.inner.RotateGrant(ctx, family, generation)
}

func (s *encryptedStore) RevokeGrant(ctx context.Context, family string) error {
	return s.inner.RevokeGrant(ctx, family)
}

func (s *encryptedStore) SweepAuthState(ctx context.Context, now time.Time) error {
	return s.inner.SweepAuthState(ctx, now)
}

type encryptedSession struct {
	inner   session.Storage
	cipher  *Cipher
	userID  tgid.UserID
	sid     string
	userKey []byte
}

func (s *encryptedSession) LoadSession(ctx context.Context) ([]byte, error) {
	blob, err := s.inner.LoadSession(ctx)
	if err != nil {
		// %w keeps session.ErrNotFound visible to errors.Is — gotd relies on
		// it to distinguish "fresh login" from a storage failure.
		return nil, fmt.Errorf("encrypted store: %w", err)
	}
	plaintext, err := s.cipher.open(s.userID, s.userKey, blob)
	if err != nil {
		// A present-but-unreadable blob (rotated-away key, issuer/AAD
		// mismatch, truncation, tampering) is NOT mapped to ErrNotFound: that
		// would read as "new user, log in fresh" and let the caller delete the
		// blob, turning a recoverable key mistake into permanent data loss.
		// ErrCorruptSession makes the fault loud and non-destructive.
		return nil, err
	}
	return plaintext, nil
}

func (s *encryptedSession) StoreSession(ctx context.Context, data []byte) error {
	if !ValidSID(s.sid) || len(s.userKey) != userKeyLen {
		return fmt.Errorf("sessionstore: refusing to store session with invalid id/key pairing")
	}
	blob, err := s.cipher.seal(s.userID, s.userKey, data)
	if err != nil {
		return err
	}
	if err := s.inner.StoreSession(ctx, blob); err != nil {
		return fmt.Errorf("encrypted store: %w", err)
	}
	return nil
}
