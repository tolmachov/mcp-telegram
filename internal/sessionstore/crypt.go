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
// vice versa.
const hkdfInfoSession = "mcp-telegram/sessionstore/aead/v1"

const masterKeyLen = 32

// ErrCorruptSession is returned by LoadSession when a stored blob exists but
// cannot be decrypted (a rotated-away key, an issuer/AAD mismatch, a
// truncated object, or tampering). It is deliberately distinct from
// session.ErrNotFound: a present-but-unreadable session is an operator error
// (usually key management), not a "log in fresh" signal, so callers must NOT
// treat it as an empty store and must NOT delete the blob on it — that would
// turn a recoverable key mistake into permanent data loss.
var ErrCorruptSession = errors.New("sessionstore: cannot decrypt session blob")

// cryptKey is one master key expanded into its AEAD. The one-byte id (first
// byte of the master key's SHA-256) prefixes every blob so decryption can
// pick the right key during rotation — same scheme as the authsrv key ring.
type cryptKey struct {
	id   byte
	aead cipher.AEAD
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
	block, err := aes.NewCipher(aeadKey)
	if err != nil {
		return nil, fmt.Errorf("creating AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}
	return &cryptKey{id: sum[0], aead: aead}, nil
}

// aad binds a blob to this deployment and user, so a blob copied to another
// user's key or another deployment fails authentication.
func (c *Cipher) aad(userID tgid.UserID) []byte {
	return []byte(c.issuer + "|session|" + userID.String())
}

// seal returns keyID || nonce || AEAD ciphertext.
func (c *Cipher) seal(userID tgid.UserID, plaintext []byte) ([]byte, error) {
	k := c.keys[0]
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}
	buf := make([]byte, 0, 1+len(nonce)+len(plaintext)+k.aead.Overhead())
	buf = append(buf, k.id)
	buf = append(buf, nonce...)
	return k.aead.Seal(buf, nonce, plaintext, c.aad(userID)), nil
}

// open reverses seal, trying the key named by the blob's key-ID byte.
func (c *Cipher) open(userID tgid.UserID, blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, ErrCorruptSession
	}
	var key *cryptKey
	for _, k := range c.keys {
		if k.id == blob[0] {
			key = k
			break
		}
	}
	if key == nil {
		return nil, fmt.Errorf("%w: sealed with a key not in the ring", ErrCorruptSession)
	}
	if len(blob) < 1+key.aead.NonceSize() {
		return nil, ErrCorruptSession
	}
	nonce := blob[1 : 1+key.aead.NonceSize()]
	plaintext, err := key.aead.Open(nil, nonce, blob[1+key.aead.NonceSize():], c.aad(userID))
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

func (s *encryptedStore) Session(userID tgid.UserID) session.Storage {
	return &encryptedSession{
		inner:  s.inner.Session(userID),
		cipher: s.cipher,
		userID: userID,
	}
}

func (s *encryptedStore) Exists(ctx context.Context, userID tgid.UserID) (bool, error) {
	ok, err := s.inner.Exists(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("encrypted store: %w", err)
	}
	return ok, nil
}

func (s *encryptedStore) Delete(ctx context.Context, userID tgid.UserID) error {
	if err := s.inner.Delete(ctx, userID); err != nil {
		return fmt.Errorf("encrypted store: %w", err)
	}
	return nil
}

type encryptedSession struct {
	inner  session.Storage
	cipher *Cipher
	userID tgid.UserID
}

func (s *encryptedSession) LoadSession(ctx context.Context) ([]byte, error) {
	blob, err := s.inner.LoadSession(ctx)
	if err != nil {
		// %w keeps session.ErrNotFound visible to errors.Is — gotd relies on
		// it to distinguish "fresh login" from a storage failure.
		return nil, fmt.Errorf("encrypted store: %w", err)
	}
	plaintext, err := s.cipher.open(s.userID, blob)
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
	blob, err := s.cipher.seal(s.userID, data)
	if err != nil {
		return err
	}
	if err := s.inner.StoreSession(ctx, blob); err != nil {
		return fmt.Errorf("encrypted store: %w", err)
	}
	return nil
}
