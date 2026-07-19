package sessionstore

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// TestStoreRejectsMismatchedPairing pins the legacy-pairing invariant enforced
// at the write: a legacy object name (empty sid) may only be sealed master-only
// (empty userKey), and a suffixed object name (non-empty sid) may only be sealed
// split-key (non-empty userKey). Either mismatch is refused before it can write
// a blob that would be unreadable on its next load.
func TestStoreRejectsMismatchedPairing(t *testing.T) {
	ctx := t.Context()
	cipher, err := NewCipher([]string{newKey(t)}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	store := Encrypted(NewMemory(), cipher)
	const user = tgid.UserID(88)
	const sid = "0123456789abcdef0123456789abcdef"

	// Legacy name + per-session key: would seal a v2 blob under the legacy name.
	if err := store.Session(user, "", userKeyForTest(t)).StoreSession(ctx, []byte("x")); err == nil {
		t.Error("storing a keyed session under the legacy (empty-sid) name must be refused")
	}
	// Suffixed name + no key: would seal a v1 blob under a suffixed name.
	if err := store.Session(user, sid, nil).StoreSession(ctx, []byte("x")); err == nil {
		t.Error("storing an unkeyed session under a suffixed name must be refused")
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

// TestRevokeTombstone exercises the tombstone lifecycle on every real backend:
// Revoke marks + deletes the blob, Revoked reflects it, tombstones are listed
// by ListRevoked but NOT by List (not mistaken for sessions), and DeleteRevoked
// clears them.
func TestRevokeTombstone(t *testing.T) {
	const user = tgid.UserID(55)
	const sid = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name string
		make func(t *testing.T) Store
	}{
		{"memory", func(_ *testing.T) Store { return NewMemory() }},
		{"fs", func(t *testing.T) Store {
			fs, err := NewFS(filepath.Join(t.TempDir(), "sessions"))
			if err != nil {
				t.Fatalf("NewFS: %v", err)
			}
			return fs
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store := tc.make(t)
			if err := store.Session(user, sid, nil).StoreSession(ctx, []byte("blob")); err != nil {
				t.Fatalf("StoreSession: %v", err)
			}

			if r, err := store.Revoked(ctx, user, sid); err != nil || r {
				t.Fatalf("Revoked before revoke = (%v, %v), want (false, nil)", r, err)
			}
			if err := store.Revoke(ctx, user, sid); err != nil {
				t.Fatalf("Revoke: %v", err)
			}
			if r, err := store.Revoked(ctx, user, sid); err != nil || !r {
				t.Errorf("Revoked after revoke = (%v, %v), want (true, nil)", r, err)
			}
			// The blob is gone; the tombstone is not listed as a session.
			if ok, _ := store.Exists(ctx, user, sid); ok {
				t.Error("Revoke must delete the session blob")
			}
			sessions, err := store.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(sessions) != 0 {
				t.Errorf("List returned %d sessions, want 0 (tombstone must not appear as a session)", len(sessions))
			}
			revoked, err := store.ListRevoked(ctx)
			if err != nil {
				t.Fatalf("ListRevoked: %v", err)
			}
			if len(revoked) != 1 || revoked[0].UserID != user || revoked[0].SID != sid {
				t.Errorf("ListRevoked = %+v, want one tombstone for (%d,%s)", revoked, user, sid)
			}

			if err := store.DeleteRevoked(ctx, user, sid); err != nil {
				t.Fatalf("DeleteRevoked: %v", err)
			}
			if r, _ := store.Revoked(ctx, user, sid); r {
				t.Error("Revoked after DeleteRevoked = true, want false")
			}
		})
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
		"",                                  // empty (legacy is handled separately, not via ValidSID)
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

// TestFSList pins the listing contract on the FS backend: legacy and suffixed
// sessions are attributed correctly, foreign files are skipped.
func TestFSList(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "sessions")
	fs, err := NewFS(dir)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}

	const sidA = "0123456789abcdef0123456789abcdef"
	if err := fs.Session(1, "", nil).StoreSession(ctx, []byte("legacy")); err != nil {
		t.Fatalf("store legacy: %v", err)
	}
	if err := fs.Session(1, sidA, nil).StoreSession(ctx, []byte("a")); err != nil {
		t.Fatalf("store a: %v", err)
	}
	if err := fs.Session(2, sidA, nil).StoreSession(ctx, []byte("b")); err != nil {
		t.Fatalf("store b: %v", err)
	}
	// Foreign files must be skipped, not attributed or failed on — including an
	// operator's backup with a numeric uid but a non-sid suffix, which the
	// sweeper must never treat as (and delete as) a session.
	for _, stray := range []string{"README.txt", "not-a-number.bin", "1.bak.bin", "1.backup-2026.bin"} {
		if err := os.WriteFile(filepath.Join(dir, stray), []byte("x"), 0o600); err != nil {
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
	want := []string{"1|", "1|" + sidA, "2|" + sidA}
	if len(refs) != len(want) {
		t.Fatalf("List returned %d refs (%v), want %d", len(refs), got, len(want))
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("List is missing %q", w)
		}
	}
}

// TestMemoryList mirrors TestFSList on the Memory backend and pins that the
// injectable clock stamps writes.
func TestMemoryList(t *testing.T) {
	ctx := t.Context()
	m := NewMemory()
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m.Now = func() time.Time { return stamp }

	if err := m.Session(7, "", nil).StoreSession(ctx, []byte("legacy")); err != nil {
		t.Fatalf("store legacy: %v", err)
	}
	if err := m.Session(7, "aa", nil).StoreSession(ctx, []byte("a")); err != nil {
		t.Fatalf("store a: %v", err)
	}

	refs, err := m.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("List returned %d refs, want 2", len(refs))
	}
	for _, r := range refs {
		if r.UserID != 7 || !r.UpdatedAt.Equal(stamp) {
			t.Errorf("ref = %+v, want user 7 at %v", r, stamp)
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
