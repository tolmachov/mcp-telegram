package sessionstore

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

const testIssuer = "https://mcp.example.com"

func newKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestCipherRoundTrip(t *testing.T) {
	c, err := NewCipher([]string{newKey(t)}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	const user = tgid.UserID(42)
	plaintext := []byte(`{"session":"data"}`)

	blob, err := c.seal(user, nil, plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := c.open(user, nil, blob)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Errorf("round trip = %q, want %q", got, plaintext)
	}
}

func TestCipherRejectsWrongUserAndKey(t *testing.T) {
	key1, key2 := newKey(t), newKey(t)
	c1, err := NewCipher([]string{key1}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	blob, err := c1.seal(1, nil, []byte("secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := c1.open(2, nil, blob); err == nil {
		t.Error("open with another user id succeeded; AAD binding is broken")
	}

	c2, err := NewCipher([]string{key2}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	if _, err := c2.open(1, nil, blob); err == nil {
		t.Error("open with a foreign key ring succeeded")
	}

	cOther, err := NewCipher([]string{key1}, "https://other.example.com")
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	if _, err := cOther.open(1, nil, blob); err == nil {
		t.Error("open under another issuer succeeded; AAD binding is broken")
	}
}

func TestCipherRotation(t *testing.T) {
	oldKey, newKeyStr := newKey(t), newKey(t)
	cOld, err := NewCipher([]string{oldKey}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	blob, err := cOld.seal(7, nil, []byte("legacy"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// New deployments list the new key first but keep the old one for reads.
	cRotated, err := NewCipher([]string{newKeyStr, oldKey}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	got, err := cRotated.open(7, nil, blob)
	if err != nil {
		t.Fatalf("open after rotation: %v", err)
	}
	if string(got) != "legacy" {
		t.Errorf("open after rotation = %q, want %q", got, "legacy")
	}
}

func TestNewCipherValidation(t *testing.T) {
	if _, err := NewCipher(nil, testIssuer); err == nil {
		t.Error("NewCipher accepted an empty key list")
	}
	if _, err := NewCipher([]string{"not-base64!"}, testIssuer); err == nil {
		t.Error("NewCipher accepted invalid base64")
	}
	short := base64.StdEncoding.EncodeToString([]byte("short"))
	if _, err := NewCipher([]string{short}, testIssuer); err == nil {
		t.Error("NewCipher accepted a short key")
	}
	k := newKey(t)
	if _, err := NewCipher([]string{k, k}, testIssuer); err == nil {
		t.Error("NewCipher accepted duplicate keys (key-ID collision)")
	}
}

func TestFSStore(t *testing.T) {
	ctx := t.Context()
	fs, err := NewFS(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	const user = tgid.UserID(100)

	if _, err := fs.Session(user, "", nil).LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("LoadSession on empty store: err = %v, want session.ErrNotFound", err)
	}
	if ok, err := fs.Exists(ctx, user, ""); err != nil || ok {
		t.Errorf("Exists on empty store = (%v, %v), want (false, nil)", ok, err)
	}

	if err := fs.Session(user, "", nil).StoreSession(ctx, []byte("blob")); err != nil {
		t.Fatalf("StoreSession: %v", err)
	}
	if ok, err := fs.Exists(ctx, user, ""); err != nil || !ok {
		t.Errorf("Exists after store = (%v, %v), want (true, nil)", ok, err)
	}
	data, err := fs.Session(user, "", nil).LoadSession(ctx)
	if err != nil || string(data) != "blob" {
		t.Errorf("LoadSession = (%q, %v), want (%q, nil)", data, err, "blob")
	}

	if err := fs.Delete(ctx, user, ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := fs.Exists(ctx, user, ""); ok {
		t.Error("Exists after delete = true, want false")
	}
	if err := fs.Delete(ctx, user, ""); err != nil {
		t.Errorf("Delete of a missing session should be a no-op, got %v", err)
	}
}

func TestEncryptedStore(t *testing.T) {
	ctx := t.Context()
	cipher, err := NewCipher([]string{newKey(t)}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	backend := NewMemory()
	store := Encrypted(backend, cipher)
	const user = tgid.UserID(5)

	if err := store.Session(user, "", nil).StoreSession(ctx, []byte("plaintext")); err != nil {
		t.Fatalf("StoreSession: %v", err)
	}

	// The backend must hold ciphertext, not the plaintext.
	raw, err := backend.Session(user, "", nil).LoadSession(ctx)
	if err != nil {
		t.Fatalf("backend LoadSession: %v", err)
	}
	if string(raw) == "plaintext" {
		t.Fatal("backend stores plaintext; encryption wrapper is not applied")
	}

	got, err := store.Session(user, "", nil).LoadSession(ctx)
	if err != nil || string(got) != "plaintext" {
		t.Errorf("LoadSession = (%q, %v), want (%q, nil)", got, err, "plaintext")
	}

	// A blob that cannot be decrypted must surface as ErrCorruptSession —
	// distinct from ErrNotFound so the caller does not mistake a key/issuer
	// misconfiguration for "new user" and destroy a recoverable session.
	if err := backend.Session(user, "", nil).StoreSession(ctx, []byte("garbage")); err != nil {
		t.Fatalf("backend StoreSession: %v", err)
	}
	_, err = store.Session(user, "", nil).LoadSession(ctx)
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
	cipher, err := NewCipher([]string{newKey(t)}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	store := Encrypted(NewMemory(), cipher)
	if _, err := store.Session(1, "", nil).LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("LoadSession on empty encrypted store: err = %v, want session.ErrNotFound", err)
	}
}

// TestFSSplitKeyNotMasterDecryptable is the end-to-end at-rest-dump check: a v2
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
	cipher, err := NewCipher([]string{key}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	store := Encrypted(backend, cipher)

	const user = tgid.UserID(77)
	const sid = "0123456789abcdef0123456789abcdef"
	uk := userKeyForTest(t)
	if err := store.Session(user, sid, uk).StoreSession(ctx, []byte("mtproto")); err != nil {
		t.Fatalf("StoreSession: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, user.String()+"."+sid+".bin")) //nolint:gosec // test temp dir + numeric id
	if err != nil {
		t.Fatalf("reading v2 session file: %v", err)
	}
	if len(raw) == 0 || raw[0] != sessionBlobV2 {
		t.Fatalf("on-disk v2 blob must start with version byte %#x, got %#v", sessionBlobV2, raw[:1])
	}

	// An attacker with the bucket + the master key but no token (empty userKey)
	// cannot read the session.
	masterOnly := Encrypted(backend, cipher)
	if _, err := masterOnly.Session(user, sid, nil).LoadSession(ctx); !errors.Is(err, ErrCorruptSession) {
		t.Errorf("master-only load of a v2 session: err = %v, want ErrCorruptSession", err)
	}
	// With the per-session key it decrypts.
	got, err := store.Session(user, sid, uk).LoadSession(ctx)
	if err != nil || string(got) != "mtproto" {
		t.Errorf("load with per-session key = (%q, %v), want (%q, nil)", got, err, "mtproto")
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

// TestCipherV2SplitKey exercises the split-key path: a v2 blob opens only with
// the exact per-session key it was sealed under, and never with a wrong key or
// with the legacy master-only path.
func TestCipherV2SplitKey(t *testing.T) {
	c, err := NewCipher([]string{newKey(t)}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	const user = tgid.UserID(42)
	uk := userKeyForTest(t)
	plaintext := []byte("mtproto-session")

	blob, err := c.seal(user, uk, plaintext)
	if err != nil {
		t.Fatalf("seal v2: %v", err)
	}
	if len(blob) == 0 || blob[0] != sessionBlobV2 {
		t.Fatalf("v2 blob must start with the version byte %#x, got %#v", sessionBlobV2, blob[:1])
	}

	got, err := c.open(user, uk, blob)
	if err != nil || string(got) != string(plaintext) {
		t.Fatalf("open v2 = (%q, %v), want (%q, nil)", got, err, plaintext)
	}

	// Wrong per-session key: the master alone is not enough.
	if _, err := c.open(user, userKeyForTest(t), blob); !errors.Is(err, ErrCorruptSession) {
		t.Errorf("open v2 with wrong user key: err = %v, want ErrCorruptSession", err)
	}
	// Empty key (legacy path) on a v2 blob: must not decrypt.
	if _, err := c.open(user, nil, blob); !errors.Is(err, ErrCorruptSession) {
		t.Errorf("open v2 with empty key: err = %v, want ErrCorruptSession", err)
	}
}

// TestCipherV1BlobRejectedWithUserKey guards the reverse cross-derivation: a
// legacy v1 blob must not be openable via the v2 path.
func TestCipherV1BlobRejectedWithUserKey(t *testing.T) {
	c, err := NewCipher([]string{newKey(t)}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	const user = tgid.UserID(9)
	blob, err := c.seal(user, nil, []byte("legacy"))
	if err != nil {
		t.Fatalf("seal v1: %v", err)
	}
	if _, err := c.open(user, userKeyForTest(t), blob); !errors.Is(err, ErrCorruptSession) {
		t.Errorf("open v1 blob via v2 path: err = %v, want ErrCorruptSession", err)
	}
}

// TestCipherV2Rotation pins that per-session v2 blobs survive a master-key
// rotation: the key-ID byte still selects the right master to re-derive from.
func TestCipherV2Rotation(t *testing.T) {
	oldKey, newKeyStr := newKey(t), newKey(t)
	cOld, err := NewCipher([]string{oldKey}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	const user = tgid.UserID(7)
	uk := userKeyForTest(t)
	blob, err := cOld.seal(user, uk, []byte("v2-legacy"))
	if err != nil {
		t.Fatalf("seal v2: %v", err)
	}
	cRotated, err := NewCipher([]string{newKeyStr, oldKey}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	got, err := cRotated.open(user, uk, blob)
	if err != nil || string(got) != "v2-legacy" {
		t.Fatalf("open v2 after rotation = (%q, %v), want (%q, nil)", got, err, "v2-legacy")
	}
}

// TestEncryptedStoreIndependentSessions pins that two authorizations of the
// same user (distinct sid + key) are stored as distinct objects and each
// decrypts only with its own key — the property that makes concurrent
// multi-client work.
func TestEncryptedStoreIndependentSessions(t *testing.T) {
	ctx := t.Context()
	cipher, err := NewCipher([]string{newKey(t)}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	backend := NewMemory()
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
