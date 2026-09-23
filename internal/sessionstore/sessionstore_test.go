package sessionstore

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/keyring"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

const testIssuer = "https://mcp.example.com"
const testSID = "0123456789abcdef0123456789abcdef"

func newKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// rotationKeys returns two fixed master keys for a ring holding both. Their
// one-byte key IDs differ (0x02 and 0x9f); two random keys would share one,
// which keyring.Parse rejects, once in 256 draws.
func rotationKeys() (oldKey, newKey string) {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x11}, keyring.MasterKeyLen)),
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x22}, keyring.MasterKeyLen))
}

// newCipher builds a session cipher over a ring parsed from encoded keys.
func newCipher(t *testing.T, issuer string, encoded ...string) *Cipher {
	t.Helper()
	ring, err := keyring.Parse(encoded)
	if err != nil {
		t.Fatalf("keyring.Parse: %v", err)
	}
	return NewCipher(ring, issuer)
}

func TestCipherRoundTrip(t *testing.T) {
	c := newCipher(t, testIssuer, newKey(t))
	const user = tgid.UserID(42)
	uk := userKeyForTest(t)
	plaintext := []byte(`{"session":"data"}`)

	blob, err := c.seal(user, uk, plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := c.open(user, uk, blob)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Errorf("round trip = %q, want %q", got, plaintext)
	}
}

// TestCipherOpensGoldenBlob pins the v3 blob format (version, key ID, nonce,
// HKDF label, AAD): a blob sealed by an earlier build under fixed keys must
// still open, so deployed sessions survive refactors of the cipher.
func TestCipherOpensGoldenBlob(t *testing.T) {
	master := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x11}, 32))
	c := newCipher(t, "https://issuer.example", master)
	blob, err := hex.DecodeString("03025a289586391d31aaef567caf76b25d1aee14031249d45fee1ba3f88e7bb6b6b09d1cbd01ac2850cd372cb8fb3837")
	if err != nil {
		t.Fatalf("decoding golden blob: %v", err)
	}
	got, err := c.open(42, bytes.Repeat([]byte{0x33}, 32), blob)
	if err != nil {
		t.Fatalf("open golden blob: %v", err)
	}
	if string(got) != `{"session":"data"}` {
		t.Errorf("golden blob = %q", got)
	}
}

func TestCipherRejectsWrongUserAndKey(t *testing.T) {
	// Fixed keys with distinct key IDs, so the foreign ring fails on the ID
	// lookup every run rather than on it or the AEAD depending on the draw.
	key1, key2 := rotationKeys()
	c1 := newCipher(t, testIssuer, key1)
	uk := userKeyForTest(t)
	blob, err := c1.seal(1, uk, []byte("secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := c1.open(2, uk, blob); err == nil {
		t.Error("open with another user id succeeded; AAD binding is broken")
	}

	c2 := newCipher(t, testIssuer, key2)
	if _, err := c2.open(1, uk, blob); err == nil {
		t.Error("open with a foreign key ring succeeded")
	}

	cOther := newCipher(t, "https://other.example.com", key1)
	if _, err := cOther.open(1, uk, blob); err == nil {
		t.Error("open under another issuer succeeded; AAD binding is broken")
	}
}

func TestCipherRotation(t *testing.T) {
	oldKey, newKeyStr := rotationKeys()
	cOld := newCipher(t, testIssuer, oldKey)
	uk := userKeyForTest(t)
	blob, err := cOld.seal(7, uk, []byte("session"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// New deployments list the new key first but keep the old one for reads.
	cRotated := newCipher(t, testIssuer, newKeyStr, oldKey)
	got, err := cRotated.open(7, uk, blob)
	if err != nil {
		t.Fatalf("open after rotation: %v", err)
	}
	if string(got) != "session" {
		t.Errorf("open after rotation = %q, want %q", got, "session")
	}
}

// newTestFS returns an FS store rooted in a fresh temporary directory.
func newTestFS(t *testing.T) *FS {
	t.Helper()
	fs, err := NewFS(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs
}

// backends returns every storage backend, fresh and empty.
func backends(t *testing.T) map[string]backend {
	t.Helper()
	return map[string]backend{"fs": newTestFS(t), "gcs": NewTestGCS(t)}
}

func TestBackendSessionLifecycle(t *testing.T) {
	for name, b := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			const user = tgid.UserID(100)

			if _, err := b.Session(user, testSID).LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
				t.Errorf("LoadSession on empty store: err = %v, want session.ErrNotFound", err)
			}
			if ok, err := b.Exists(ctx, user, testSID); err != nil || ok {
				t.Errorf("Exists on empty store = (%v, %v), want (false, nil)", ok, err)
			}

			if err := b.Session(user, testSID).StoreSession(ctx, []byte("blob")); err != nil {
				t.Fatalf("StoreSession: %v", err)
			}
			if ok, err := b.Exists(ctx, user, testSID); err != nil || !ok {
				t.Errorf("Exists after store = (%v, %v), want (true, nil)", ok, err)
			}
			data, err := b.Session(user, testSID).LoadSession(ctx)
			if err != nil || string(data) != "blob" {
				t.Errorf("LoadSession = (%q, %v), want (%q, nil)", data, err, "blob")
			}

			if err := b.Delete(ctx, user, testSID); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if ok, _ := b.Exists(ctx, user, testSID); ok {
				t.Error("Exists after delete = true, want false")
			}
			if err := b.Delete(ctx, user, testSID); err != nil {
				t.Errorf("Delete of a missing session should be a no-op, got %v", err)
			}
		})
	}
}

// TestBackendZeroByteBlob pins backend parity for a truncated (0-byte) session
// blob: Exists and LoadSession treat it as absent, so the refresh Exists gate
// never passes a session the client build would then reject, while List still
// reports it and Delete removes it, so the sweeper can reclaim it.
func TestBackendZeroByteBlob(t *testing.T) {
	for name, b := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			const user = tgid.UserID(100)
			if err := b.Session(user, testSID).StoreSession(ctx, nil); err != nil {
				t.Fatalf("seeding a 0-byte session: %v", err)
			}
			if ok, err := b.Exists(ctx, user, testSID); err != nil || ok {
				t.Errorf("Exists on a 0-byte session = (%v, %v), want (false, nil)", ok, err)
			}
			if _, err := b.Session(user, testSID).LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
				t.Errorf("LoadSession on a 0-byte session: err = %v, want session.ErrNotFound", err)
			}
			refs, err := b.List(ctx)
			if err != nil || len(refs) != 1 || refs[0].UserID != user || refs[0].SID != testSID {
				t.Fatalf("List = (%+v, %v), want the 0-byte session", refs, err)
			}
			if err := b.Delete(ctx, user, testSID); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if refs, err := b.List(ctx); err != nil || len(refs) != 0 {
				t.Errorf("List after Delete = (%+v, %v), want empty", refs, err)
			}
		})
	}
}

// TestListSkipsNonCanonicalNames pins the sweeper-safety invariant that List
// only attributes objects this package itself could have written. tgid.Parse
// accepts non-canonical spellings ("07", "+7") that sessionBase never emits;
// without the round-trip check a foreign 07.bin would alias to user 7, and the
// sweeper — aging the foreign file but deleting by canonical name — would
// destroy the live 7.bin (or, mirrored under revoked/, a real tombstone).
func TestListSkipsNonCanonicalNames(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "sessions")
	fs, err := NewFS(dir)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	const user = tgid.UserID(7)
	if err := fs.Session(user, testSID).StoreSession(ctx, []byte("live")); err != nil {
		t.Fatalf("StoreSession: %v", err)
	}
	for _, foreign := range []string{"07.bin", "+7.bin"} {
		if err := os.WriteFile(filepath.Join(fs.sessionsDir(), foreign), []byte("foreign"), 0o600); err != nil {
			t.Fatalf("seeding %s: %v", foreign, err)
		}
	}

	refs, err := fs.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 || refs[0].UserID != user || refs[0].SID != testSID {
		t.Errorf("List = %+v, want exactly the canonical current-format ref", refs)
	}
}

func TestEncryptedStore(t *testing.T) {
	ctx := t.Context()
	cipher := newCipher(t, testIssuer, newKey(t))
	backend := newTestFS(t)
	store := Encrypted(backend, cipher)
	const user = tgid.UserID(5)
	uk := userKeyForTest(t)

	if err := store.Session(user, testSID, uk).StoreSession(ctx, []byte("plaintext")); err != nil {
		t.Fatalf("StoreSession: %v", err)
	}

	// The backend must hold ciphertext, not the plaintext.
	raw, err := backend.Session(user, testSID).LoadSession(ctx)
	if err != nil {
		t.Fatalf("backend LoadSession: %v", err)
	}
	if string(raw) == "plaintext" {
		t.Fatal("backend stores plaintext; encryption wrapper is not applied")
	}

	got, err := store.Session(user, testSID, uk).LoadSession(ctx)
	if err != nil || string(got) != "plaintext" {
		t.Errorf("LoadSession = (%q, %v), want (%q, nil)", got, err, "plaintext")
	}

	// A blob that cannot be decrypted must surface as ErrCorruptSession —
	// distinct from ErrNotFound so the caller does not mistake a key/issuer
	// misconfiguration for "new user" and destroy a recoverable session.
	if err := backend.Session(user, testSID).StoreSession(ctx, []byte("garbage")); err != nil {
		t.Fatalf("backend StoreSession: %v", err)
	}
	_, err = store.Session(user, testSID, uk).LoadSession(ctx)
	if !errors.Is(err, ErrCorruptSession) {
		t.Errorf("LoadSession of corrupt blob: err = %v, want ErrCorruptSession", err)
	}
	if errors.Is(err, session.ErrNotFound) {
		t.Error("corrupt blob must NOT be reported as ErrNotFound (would trigger destructive delete)")
	}
}

// TestEncryptedStorePreservesErrNotFound pins that a genuinely empty backend
// still surfaces session.ErrNotFound through the wrapper, so gotd starts a
// fresh login for a new user instead of erroring the build.
func TestEncryptedStorePreservesErrNotFound(t *testing.T) {
	ctx := t.Context()
	cipher := newCipher(t, testIssuer, newKey(t))
	store := Encrypted(newTestFS(t), cipher)
	if _, err := store.Session(1, testSID, userKeyForTest(t)).LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("LoadSession on empty encrypted store: err = %v, want session.ErrNotFound", err)
	}
}

// TestFSSplitKeyNotMasterDecryptable is the end-to-end at-rest-dump check: a v3
// session written to disk begins with the version byte and cannot be decrypted
// by a store that holds only the master key (no per-session key). This is the
// whole point — a leaked bucket + secret, without a live token, is insufficient.
func TestFSSplitKeyNotMasterDecryptable(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "sessions")
	key := newKey(t)

	backend, err := NewFS(dir)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	cipher := newCipher(t, testIssuer, key)
	store := Encrypted(backend, cipher)

	const user = tgid.UserID(77)
	const sid = "0123456789abcdef0123456789abcdef"
	uk := userKeyForTest(t)
	if err := store.Session(user, sid, uk).StoreSession(ctx, []byte("mtproto")); err != nil {
		t.Fatalf("StoreSession: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(backend.sessionsDir(), user.String()+"."+sid+".bin")) //nolint:gosec // test temp dir + numeric id
	if err != nil {
		t.Fatalf("reading v3 session file: %v", err)
	}
	if len(raw) == 0 || raw[0] != sessionBlobVersion {
		t.Fatalf("on-disk v3 blob must start with version byte %#x, got %#v", sessionBlobVersion, raw[:1])
	}

	// An attacker with the bucket + the master key but no token cannot read
	// the session: a guessed per-session key does not decrypt it, and a
	// missing one is refused outright.
	if _, err := store.Session(user, sid, userKeyForTest(t)).LoadSession(ctx); !errors.Is(err, ErrCorruptSession) {
		t.Errorf("load of a v3 session with a guessed key: err = %v, want ErrCorruptSession", err)
	}
	if _, err := store.Session(user, sid, nil).LoadSession(ctx); !errors.Is(err, ErrInvalidSessionKey) {
		t.Errorf("master-only load of a v3 session: err = %v, want ErrInvalidSessionKey", err)
	}
	// With the per-session key it decrypts.
	got, err := store.Session(user, sid, uk).LoadSession(ctx)
	if err != nil || string(got) != "mtproto" {
		t.Errorf("load with per-session key = (%q, %v), want (%q, nil)", got, err, "mtproto")
	}
}

func TestStoreRejectsMissingSessionIdentity(t *testing.T) {
	ctx := t.Context()
	cipher := newCipher(t, testIssuer, newKey(t))
	store := Encrypted(newTestFS(t), cipher)
	const user = tgid.UserID(88)
	const sid = "0123456789abcdef0123456789abcdef"

	if err := store.Session(user, "", userKeyForTest(t)).StoreSession(ctx, []byte("x")); err == nil {
		t.Error("storing with an empty session id must be refused")
	}
	if err := store.Session(user, sid, nil).StoreSession(ctx, []byte("x")); err == nil {
		t.Error("storing without a per-session key must be refused")
	}
	// Neither mismatched write may have landed.
	refs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("no object may be written on a pairing mismatch, got %d", len(refs))
	}
}

// TestEncryptedStoreRejectsWrongLengthSessionKey pins that a per-session key
// must be exactly the minted length: a short or oversized key fails closed on
// both store and load, before any blob is sealed or opened, and with its own
// error rather than ErrCorruptSession, which would blame the stored blob or the
// master keys.
func TestEncryptedStoreRejectsWrongLengthSessionKey(t *testing.T) {
	ctx := t.Context()
	store := Encrypted(newTestFS(t), newCipher(t, testIssuer, newKey(t)))
	const user = tgid.UserID(91)
	if err := store.Session(user, testSID, userKeyForTest(t)).StoreSession(ctx, []byte("mtproto")); err != nil {
		t.Fatalf("StoreSession with a valid key: %v", err)
	}
	for _, bad := range [][]byte{nil, make([]byte, 1), make([]byte, 16), make([]byte, 31), make([]byte, 33)} {
		if err := store.Session(user, testSID, bad).StoreSession(ctx, []byte("x")); !errors.Is(err, ErrInvalidSessionKey) {
			t.Errorf("StoreSession with a %d-byte key: err = %v, want ErrInvalidSessionKey", len(bad), err)
		}
		_, err := store.Session(user, testSID, bad).LoadSession(ctx)
		if !errors.Is(err, ErrInvalidSessionKey) || errors.Is(err, ErrCorruptSession) {
			t.Errorf("LoadSession with a %d-byte key: err = %v, want ErrInvalidSessionKey only", len(bad), err)
		}
	}
}

// TestNewSessionIdentityIsValid guards against drift between minting and
// validation: a minted sid, family or key must always validate, or every fresh
// session would fail on its first use.
func TestNewSessionIdentityIsValid(t *testing.T) {
	sid, other := NewSID(), NewSID()
	if !ValidSID(sid) {
		t.Errorf("NewSID() = %q, which ValidSID rejects", sid)
	}
	if sid == other {
		t.Errorf("two NewSID calls returned the same id %q", sid)
	}
	if key := NewSessionKey(); !ValidSessionKey(key) {
		t.Errorf("NewSessionKey() returned %d bytes, which ValidSessionKey rejects", len(key))
	}
}

// TestEncryptedStoreRejectsInvalidSID pins the store-boundary guard: a non-empty
// sid that is not a valid session id is refused by every method that turns it
// into an object name, so a malformed value can never build a storage path.
func TestEncryptedStoreRejectsInvalidSID(t *testing.T) {
	ctx := t.Context()
	cipher := newCipher(t, testIssuer, newKey(t))
	store := Encrypted(newTestFS(t), cipher)
	const user = tgid.UserID(92)
	const bad = "../escape" // non-empty, not ValidSID

	if _, err := store.Session(user, bad, userKeyForTest(t)).LoadSession(ctx); err == nil {
		t.Error("Session(invalid sid).LoadSession must fail")
	}
	if err := store.Session(user, bad, userKeyForTest(t)).StoreSession(ctx, []byte("x")); err == nil {
		t.Error("Session(invalid sid).StoreSession must fail")
	}
	if _, err := store.Exists(ctx, user, bad); err == nil {
		t.Error("Exists(invalid sid) must fail")
	}
	if err := store.Delete(ctx, user, bad); err == nil {
		t.Error("Delete(invalid sid) must fail")
	}
	if err := store.Revoke(ctx, user, bad); err == nil {
		t.Error("Revoke(invalid sid) must fail")
	}
	if _, err := store.Revoked(ctx, user, bad); err == nil {
		t.Error("Revoked(invalid sid) must fail")
	}
	if _, err := store.Exists(ctx, user, ""); err == nil {
		t.Error("Exists(empty sid) must fail")
	}
	if _, err := store.Exists(ctx, user, "0123456789abcdef0123456789abcdef"); err != nil {
		t.Errorf("Exists(valid sid) must be accepted: %v", err)
	}
}

func TestValidSID(t *testing.T) {
	valid := []string{
		"0123456789abcdef0123456789abcdef",
		"ffffffffffffffffffffffffffffffff",
	}
	for _, s := range valid {
		if !ValidSID(s) {
			t.Errorf("ValidSID(%q) = false, want true", s)
		}
	}
	invalid := []string{
		"",                                  // empty ids are never valid
		"0123456789abcdef0123456789abcde",   // 31 chars
		"0123456789abcdef0123456789abcdef0", // 33 chars
		"0123456789ABCDEF0123456789abcdef",  // uppercase
		"0123456789abcdef0123456789abcdeg",  // non-hex 'g'
		"../../etc/passwd",                  // path traversal
		"bak",                               // operator suffix
	}
	for _, s := range invalid {
		if ValidSID(s) {
			t.Errorf("ValidSID(%q) = true, want false", s)
		}
	}
}

// TestFSList pins the listing contract on the FS backend.
func TestFSList(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "sessions")
	fs, err := NewFS(dir)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}

	const sidA = "0123456789abcdef0123456789abcdef"
	if err := fs.Session(1, sidA).StoreSession(ctx, []byte("a")); err != nil {
		t.Fatalf("store a: %v", err)
	}
	if err := fs.Session(2, sidA).StoreSession(ctx, []byte("b")); err != nil {
		t.Fatalf("store b: %v", err)
	}
	// Foreign files must be skipped, not attributed or failed on — including an
	// operator's backup with a numeric uid but a non-sid suffix, which the
	// sweeper must never treat as (and delete as) a session.
	for _, stray := range []string{"README.txt", "not-a-number.bin", "1.bak.bin", "1.backup-2026.bin"} {
		if err := os.WriteFile(filepath.Join(fs.sessionsDir(), stray), []byte("x"), 0o600); err != nil {
			t.Fatalf("writing stray file %s: %v", stray, err)
		}
	}

	refs, err := fs.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]bool{}
	for _, r := range refs {
		got[r.UserID.String()+"|"+r.SID] = true
		if r.UpdatedAt.IsZero() {
			t.Errorf("ref %v has zero UpdatedAt", r)
		}
	}
	want := []string{"1|" + sidA, "2|" + sidA}
	if len(refs) != len(want) {
		t.Fatalf("List returned %d refs (%v), want %d", len(refs), got, len(want))
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("List is missing %q", w)
		}
	}
}

func userKeyForTest(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("generating user key: %v", err)
	}
	return k
}

func TestCipherV3SplitKey(t *testing.T) {
	c := newCipher(t, testIssuer, newKey(t))
	const user = tgid.UserID(42)
	uk := userKeyForTest(t)
	plaintext := []byte("mtproto-session")

	blob, err := c.seal(user, uk, plaintext)
	if err != nil {
		t.Fatalf("seal v3: %v", err)
	}
	if len(blob) == 0 || blob[0] != sessionBlobVersion {
		t.Fatalf("v3 blob must start with the version byte %#x, got %#v", sessionBlobVersion, blob[:1])
	}

	got, err := c.open(user, uk, blob)
	if err != nil || string(got) != string(plaintext) {
		t.Fatalf("open v3 = (%q, %v), want (%q, nil)", got, err, plaintext)
	}

	// Wrong per-session key: the master alone is not enough.
	if _, err := c.open(user, userKeyForTest(t), blob); !errors.Is(err, ErrCorruptSession) {
		t.Errorf("open v3 with wrong user key: err = %v, want ErrCorruptSession", err)
	}
	// A missing split-key share must never decrypt a v3 blob.
	if _, err := c.open(user, nil, blob); !errors.Is(err, ErrCorruptSession) {
		t.Errorf("open v3 with empty key: err = %v, want ErrCorruptSession", err)
	}
}

// TestCipherV3WrongUserRejected pins the v3 AAD's userID binding on its own:
// the SAME per-session key opening another user's blob must fail. The v3 key
// derivation does not involve the user id at all, so only the AAD stands
// between a copied blob and a cross-user decrypt — dropping userID from the v3
// AAD would pass every other test.
func TestCipherV3WrongUserRejected(t *testing.T) {
	c := newCipher(t, testIssuer, newKey(t))
	uk := userKeyForTest(t)
	blob, err := c.seal(tgid.UserID(42), uk, []byte("mtproto-session"))
	if err != nil {
		t.Fatalf("seal v3: %v", err)
	}
	if _, err := c.open(tgid.UserID(43), uk, blob); !errors.Is(err, ErrCorruptSession) {
		t.Errorf("open user 42's v3 blob as user 43 with the same key: err = %v, want ErrCorruptSession", err)
	}
}

func TestCipherOldBlobRejected(t *testing.T) {
	c := newCipher(t, testIssuer, newKey(t))
	oldBlob := append([]byte{c.ring.Primary().ID}, make([]byte, 64)...)
	if _, err := c.open(tgid.UserID(9), userKeyForTest(t), oldBlob); !errors.Is(err, ErrCorruptSession) {
		t.Errorf("open old blob: err = %v, want ErrCorruptSession", err)
	}
}

// TestCipherV3Rotation pins that per-session v3 blobs survive a master-key
// rotation: the key-ID byte still selects the right master to re-derive from.
func TestCipherV3Rotation(t *testing.T) {
	oldKey, newKeyStr := rotationKeys()
	cOld := newCipher(t, testIssuer, oldKey)
	const user = tgid.UserID(7)
	uk := userKeyForTest(t)
	blob, err := cOld.seal(user, uk, []byte("v3-current"))
	if err != nil {
		t.Fatalf("seal v3: %v", err)
	}
	cRotated := newCipher(t, testIssuer, newKeyStr, oldKey)
	got, err := cRotated.open(user, uk, blob)
	if err != nil || string(got) != "v3-current" {
		t.Fatalf("open v3 after rotation = (%q, %v), want (%q, nil)", got, err, "v3-current")
	}
}

// TestEncryptedStoreIndependentSessions pins that two authorizations of the
// same user (distinct sid + key) are stored as distinct objects and each
// decrypts only with its own key — the property that makes concurrent
// multi-client work.
func TestEncryptedStoreIndependentSessions(t *testing.T) {
	ctx := t.Context()
	cipher := newCipher(t, testIssuer, newKey(t))
	backend := newTestFS(t)
	store := Encrypted(backend, cipher)
	const user = tgid.UserID(5)
	sidA, keyA := "0123456789abcdef0123456789abcdef", userKeyForTest(t)
	sidB, keyB := "fedcba9876543210fedcba9876543210", userKeyForTest(t)

	if err := store.Session(user, sidA, keyA).StoreSession(ctx, []byte("session-A")); err != nil {
		t.Fatalf("store A: %v", err)
	}
	if err := store.Session(user, sidB, keyB).StoreSession(ctx, []byte("session-B")); err != nil {
		t.Fatalf("store B: %v", err)
	}

	gotA, err := store.Session(user, sidA, keyA).LoadSession(ctx)
	if err != nil || string(gotA) != "session-A" {
		t.Errorf("load A = (%q, %v), want (%q, nil)", gotA, err, "session-A")
	}
	gotB, err := store.Session(user, sidB, keyB).LoadSession(ctx)
	if err != nil || string(gotB) != "session-B" {
		t.Errorf("load B = (%q, %v), want (%q, nil)", gotB, err, "session-B")
	}

	// A's key must not open B's object (encodes the whole point: each session's
	// blob is bound to its own key).
	if _, err := store.Session(user, sidB, keyA).LoadSession(ctx); !errors.Is(err, ErrCorruptSession) {
		t.Errorf("load B with A's key: err = %v, want ErrCorruptSession", err)
	}
}
