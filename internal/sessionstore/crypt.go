package sessionstore

import (
	"context"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/keyring"
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

// Cipher encrypts session blobs with the ring's first key and decrypts with
// any. Every blob is prefixed with the sealing key's ID (see keyring.Key) so
// decryption can pick the right key during rotation. AEADs are derived per
// (master, userKey) on demand.
type Cipher struct {
	ring   *keyring.Ring
	issuer string
}

// NewCipher builds a session cipher over the MCP_AUTH_TOKEN_KEYS ring. issuer
// participates in the AAD so blobs cannot travel between deployments.
func NewCipher(ring *keyring.Ring, issuer string) *Cipher {
	return &Cipher{ring: ring, issuer: issuer}
}

// aeadFor returns the AEAD derived from both the master and per-session key.
func (c *Cipher) aeadFor(k keyring.Key, userKey []byte) (cipher.AEAD, error) {
	if len(userKey) != userKeyLen {
		return nil, fmt.Errorf("sessionstore: session key must be %d bytes, got %d", userKeyLen, len(userKey))
	}
	aeadKey, err := hkdf.Key(sha256.New, k.Master, userKey, hkdfInfoSession, 32)
	if err != nil {
		return nil, fmt.Errorf("deriving v3 AEAD key: %w", err)
	}
	aead, err := keyring.NewGCM(aeadKey)
	if err != nil {
		return nil, fmt.Errorf("session AEAD: %w", err)
	}
	return aead, nil
}

// aad binds a blob to this deployment, format, and user.
func (c *Cipher) aad(userID tgid.UserID) []byte {
	return []byte(c.issuer + "|session|v3|" + userID.String())
}

// seal returns version || keyID || nonce || AEAD ciphertext.
func (c *Cipher) seal(userID tgid.UserID, userKey, plaintext []byte) ([]byte, error) {
	k := c.ring.Primary()
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
	buf = append(buf, k.ID)
	buf = append(buf, nonce...)
	return aead.Seal(buf, nonce, plaintext, c.aad(userID)), nil
}

// open reverses seal. Unsupported formats are rejected without migration.
func (c *Cipher) open(userID tgid.UserID, userKey, blob []byte) ([]byte, error) {
	if len(blob) < 2 || blob[0] != sessionBlobVersion {
		return nil, fmt.Errorf("%w: unsupported session blob version", ErrCorruptSession)
	}
	key, ok := c.ring.ByID(blob[1])
	if !ok {
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

// Encrypted wraps a backend so that it only ever sees AEAD ciphertext. It is
// also the one place session ids and grant families are validated: a
// token-derived value must not become an object-name or file-path suffix
// unchecked, and the backends trust what reaches them.
func Encrypted(inner Store, cipher *Cipher) Store {
	return &encryptedStore{inner: inner, cipher: cipher}
}

type encryptedStore struct {
	inner  Store
	cipher *Cipher
}

// ErrInvalidSID is returned by the Encrypted store when a caller presents a
// malformed session id or grant family (see ValidSID).
var ErrInvalidSID = errors.New("sessionstore: invalid session id")

// brokenSession is returned by Session for an invalid sid; every operation
// fails with the same error so the mismatch surfaces immediately instead of
// building a path from unvalidated input.
type brokenSession struct{ err error }

func (b brokenSession) LoadSession(context.Context) ([]byte, error) { return nil, b.err }
func (b brokenSession) StoreSession(context.Context, []byte) error  { return b.err }

func (s *encryptedStore) Session(userID tgid.UserID, sid string, userKey []byte) session.Storage {
	if !ValidSID(sid) {
		return brokenSession{err: ErrInvalidSID}
	}
	return &encryptedSession{
		inner:   s.inner.Session(userID, sid, nil),
		cipher:  s.cipher,
		userID:  userID,
		userKey: userKey,
	}
}

func (s *encryptedStore) Exists(ctx context.Context, userID tgid.UserID, sid string) (bool, error) {
	if !ValidSID(sid) {
		return false, ErrInvalidSID
	}
	ok, err := s.inner.Exists(ctx, userID, sid)
	if err != nil {
		return false, fmt.Errorf("encrypted store: %w", err)
	}
	return ok, nil
}

func (s *encryptedStore) Delete(ctx context.Context, userID tgid.UserID, sid string) error {
	if !ValidSID(sid) {
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
	if !ValidSID(sid) {
		return ErrInvalidSID
	}
	if err := s.inner.Revoke(ctx, userID, sid); err != nil {
		return fmt.Errorf("encrypted store: %w", err)
	}
	return nil
}

func (s *encryptedStore) Revoked(ctx context.Context, userID tgid.UserID, sid string) (bool, error) {
	if !ValidSID(sid) {
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
	if !ValidSID(sid) {
		return ErrInvalidSID
	}
	if err := s.inner.DeleteRevoked(ctx, userID, sid); err != nil {
		return fmt.Errorf("encrypted store: %w", err)
	}
	return nil
}

// Grant records carry no secret either (a session id and counters), so they
// pass straight through too.

func (s *encryptedStore) LoadGrant(ctx context.Context, family string) (GrantRecord, int64, error) {
	if !ValidSID(family) {
		return GrantRecord{}, 0, ErrInvalidSID
	}
	grant, version, err := s.inner.LoadGrant(ctx, family)
	if err != nil {
		return GrantRecord{}, 0, fmt.Errorf("encrypted store: %w", err)
	}
	return grant, version, nil
}

func (s *encryptedStore) StoreGrant(ctx context.Context, family string, grant GrantRecord, version int64) error {
	if !ValidSID(family) || !ValidSID(grant.SID) {
		return ErrInvalidSID
	}
	if err := s.inner.StoreGrant(ctx, family, grant, version); err != nil {
		return fmt.Errorf("encrypted store: %w", err)
	}
	return nil
}

func (s *encryptedStore) SweepAuthState(ctx context.Context, now time.Time) error {
	if err := s.inner.SweepAuthState(ctx, now); err != nil {
		return fmt.Errorf("encrypted store: %w", err)
	}
	return nil
}

type encryptedSession struct {
	inner   session.Storage
	cipher  *Cipher
	userID  tgid.UserID
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

// StoreSession seals data first; aeadFor refuses a missing or short userKey,
// so nothing is written without a proper per-session key.
func (s *encryptedSession) StoreSession(ctx context.Context, data []byte) error {
	blob, err := s.cipher.seal(s.userID, s.userKey, data)
	if err != nil {
		return err
	}
	if err := s.inner.StoreSession(ctx, blob); err != nil {
		return fmt.Errorf("encrypted store: %w", err)
	}
	return nil
}
