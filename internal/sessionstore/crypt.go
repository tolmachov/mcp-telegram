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

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// hkdfInfoSession domain-separates the session-encryption subkey from the
// authsrv token subkeys derived from the same AUTH_TOKEN_KEY master keys: a
// component that can decrypt sessions must not be able to mint tokens and
// vice versa. The v1 label derives the legacy master-only key (empty userKey);
// the v2 label derives the split key that additionally folds in the per-session
// userKey via the HKDF salt.
const (
	hkdfInfoSession   = "mcp-telegram/sessionstore/aead/v1"
	hkdfInfoSessionV2 = "mcp-telegram/sessionstore/aead/v2"
)

// sessionBlobV2 is the first byte of a v2 (split-key) blob. Legacy v1 blobs
// have no version byte and begin directly with the key ID; a v1 key ID can be
// any byte, so v1 and v2 blobs are told apart by whether a userKey is present
// (they are always minted together), not by sniffing this byte.
const sessionBlobV2 = 0x02

const masterKeyLen = 32

// userKeyLen is the required length of a v2 per-session key (matches the key
// authsrv mints). Enforced in aeadFor so a short/low-entropy key can never seal
// or open a v2 blob — the split-key protection must not silently degrade if an
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
// rotation — same scheme as the authsrv key ring. v1AEAD is the cached
// legacy (master-only) AEAD; v2 AEADs are derived per (master, userKey) on
// demand, so master is retained for that derivation.
type cryptKey struct {
	id     byte
	master []byte
	v1AEAD cipher.AEAD
}

// Cipher encrypts session blobs with the first key and decrypts with any.
type Cipher struct {
	keys   []*cryptKey
	issuer string
}

// NewCipher parses base64-encoded 32-byte master keys (the AUTH_TOKEN_KEY
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
		k, err := deriveCryptKey(master)
		if err != nil {
			return nil, fmt.Errorf("sessionstore key %d: %w", i, err)
		}
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

func deriveCryptKey(master []byte) (*cryptKey, error) {
	sum := sha256.Sum256(master)
	aeadKey, err := hkdf.Key(sha256.New, master, nil, hkdfInfoSession, 32)
	if err != nil {
		return nil, fmt.Errorf("deriving AEAD key: %w", err)
	}
	aead, err := newGCM(aeadKey)
	if err != nil {
		return nil, err
	}
	m := make([]byte, len(master))
	copy(m, master)
	return &cryptKey{id: sum[0], master: m, v1AEAD: aead}, nil
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

// aeadFor returns the AEAD for one blob. With no userKey it is the cached
// legacy (master-only) AEAD; with a userKey it is the v2 key derived by folding
// the per-session userKey into the HKDF salt, so the resulting key depends on
// BOTH the master and userKey.
func (c *Cipher) aeadFor(k *cryptKey, userKey []byte) (cipher.AEAD, error) {
	if len(userKey) == 0 {
		return k.v1AEAD, nil
	}
	if len(userKey) != userKeyLen {
		return nil, fmt.Errorf("sessionstore: v2 session key must be %d bytes, got %d", userKeyLen, len(userKey))
	}
	aeadKey, err := hkdf.Key(sha256.New, k.master, userKey, hkdfInfoSessionV2, 32)
	if err != nil {
		return nil, fmt.Errorf("deriving v2 AEAD key: %w", err)
	}
	return newGCM(aeadKey)
}

// aad binds a blob to this deployment and user, so a blob copied to another
// user's key or another deployment fails authentication. The v2 variant also
// binds the blob version, so a v1 and v2 blob can never be confused.
func (c *Cipher) aad(userID tgid.UserID, v2 bool) []byte {
	if v2 {
		return []byte(c.issuer + "|session|v2|" + userID.String())
	}
	return []byte(c.issuer + "|session|" + userID.String())
}

// seal returns [0x02 ||] keyID || nonce || AEAD ciphertext — the leading
// version byte is present only for v2 (non-empty userKey) blobs.
func (c *Cipher) seal(userID tgid.UserID, userKey, plaintext []byte) ([]byte, error) {
	k := c.keys[0]
	v2 := len(userKey) > 0
	aead, err := c.aeadFor(k, userKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}
	header := 1 // keyID
	if v2 {
		header = 2 // version byte + keyID
	}
	buf := make([]byte, 0, header+len(nonce)+len(plaintext)+aead.Overhead())
	if v2 {
		buf = append(buf, sessionBlobV2)
	}
	buf = append(buf, k.id)
	buf = append(buf, nonce...)
	return aead.Seal(buf, nonce, plaintext, c.aad(userID, v2)), nil
}

// open reverses seal. The userKey (present iff the blob is v2) selects the
// derivation and the blob layout; v2 blobs must carry the version byte.
func (c *Cipher) open(userID tgid.UserID, userKey, blob []byte) ([]byte, error) {
	v2 := len(userKey) > 0
	idOffset := 0
	if v2 {
		if len(blob) < 1 || blob[0] != sessionBlobV2 {
			return nil, fmt.Errorf("%w: expected a v2 session blob", ErrCorruptSession)
		}
		idOffset = 1
	}
	if len(blob) < idOffset+1 {
		return nil, ErrCorruptSession
	}
	keyID := blob[idOffset]
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
		return nil, err
	}
	headerLen := idOffset + 1 + aead.NonceSize()
	if len(blob) < headerLen {
		return nil, ErrCorruptSession
	}
	nonce := blob[idOffset+1 : headerLen]
	plaintext, err := aead.Open(nil, nonce, blob[headerLen:], c.aad(userID, v2))
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

// validStoreSID guards the store boundary: a non-empty sid becomes an
// object-name / file-path suffix, so a malformed one (callers should already
// have rejected it) must never reach a backend. Empty sid is the legacy
// per-user object. This is defense-in-depth — every production caller validates
// upstream — against a future path that forwards a token sid unchecked.
func validStoreSID(sid string) bool { return sid == "" || ValidSID(sid) }

// errInvalidStoreSID is returned by every encryptedStore method when a caller
// presents a malformed non-empty sid (see validStoreSID).
var errInvalidStoreSID = errors.New("sessionstore: invalid session id")

// brokenSession is returned by Session for an invalid sid; every operation
// fails with the same error so the mismatch surfaces immediately instead of
// building a path from unvalidated input.
type brokenSession struct{ err error }

func (b brokenSession) LoadSession(context.Context) ([]byte, error) { return nil, b.err }
func (b brokenSession) StoreSession(context.Context, []byte) error  { return b.err }

func (s *encryptedStore) Session(userID tgid.UserID, sid string, userKey []byte) session.Storage {
	if !validStoreSID(sid) {
		return brokenSession{err: errInvalidStoreSID}
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
		return false, errInvalidStoreSID
	}
	ok, err := s.inner.Exists(ctx, userID, sid)
	if err != nil {
		return false, fmt.Errorf("encrypted store: %w", err)
	}
	return ok, nil
}

func (s *encryptedStore) Delete(ctx context.Context, userID tgid.UserID, sid string) error {
	if !validStoreSID(sid) {
		return errInvalidStoreSID
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
		return errInvalidStoreSID
	}
	if err := s.inner.Revoke(ctx, userID, sid); err != nil {
		return fmt.Errorf("encrypted store: %w", err)
	}
	return nil
}

func (s *encryptedStore) Revoked(ctx context.Context, userID tgid.UserID, sid string) (bool, error) {
	if !validStoreSID(sid) {
		return false, errInvalidStoreSID
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
		return errInvalidStoreSID
	}
	if err := s.inner.DeleteRevoked(ctx, userID, sid); err != nil {
		return fmt.Errorf("encrypted store: %w", err)
	}
	return nil
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
	// Legacy-pairing invariant, enforced at the write: a legacy session (empty
	// sid) is master-only (empty userKey) and a suffixed session (non-empty sid)
	// is split-key (non-empty userKey) — the two are always minted together, so a
	// mismatch is an upstream programming error. Refuse to persist it rather than
	// seal a v2 blob under the legacy object name (or a v1 blob under a suffixed
	// name), which would silently strand the session as ErrCorruptSession on its
	// next read. Reads are left to fail naturally as ErrCorruptSession, which also
	// covers the legitimate "attacker probes a v2 blob master-only" path.
	if (s.sid == "") != (len(s.userKey) == 0) {
		return fmt.Errorf(
			"sessionstore: refusing to store session with mismatched id/key pairing (both must be empty for legacy, both set otherwise)")
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
