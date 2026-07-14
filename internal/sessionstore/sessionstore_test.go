package sessionstore

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
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

	blob, err := c.seal(user, plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	got, err := c.open(user, blob)
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
	blob, err := c1.seal(1, []byte("secret"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := c1.open(2, blob); err == nil {
		t.Error("open with another user id succeeded; AAD binding is broken")
	}

	c2, err := NewCipher([]string{key2}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	if _, err := c2.open(1, blob); err == nil {
		t.Error("open with a foreign key ring succeeded")
	}

	cOther, err := NewCipher([]string{key1}, "https://other.example.com")
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	if _, err := cOther.open(1, blob); err == nil {
		t.Error("open under another issuer succeeded; AAD binding is broken")
	}
}

func TestCipherRotation(t *testing.T) {
	oldKey, newKeyStr := newKey(t), newKey(t)
	cOld, err := NewCipher([]string{oldKey}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	blob, err := cOld.seal(7, []byte("legacy"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// New deployments list the new key first but keep the old one for reads.
	cRotated, err := NewCipher([]string{newKeyStr, oldKey}, testIssuer)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	got, err := cRotated.open(7, blob)
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

	if _, err := fs.Session(user).LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("LoadSession on empty store: err = %v, want session.ErrNotFound", err)
	}
	if ok, err := fs.Exists(ctx, user); err != nil || ok {
		t.Errorf("Exists on empty store = (%v, %v), want (false, nil)", ok, err)
	}

	if err := fs.Session(user).StoreSession(ctx, []byte("blob")); err != nil {
		t.Fatalf("StoreSession: %v", err)
	}
	if ok, err := fs.Exists(ctx, user); err != nil || !ok {
		t.Errorf("Exists after store = (%v, %v), want (true, nil)", ok, err)
	}
	data, err := fs.Session(user).LoadSession(ctx)
	if err != nil || string(data) != "blob" {
		t.Errorf("LoadSession = (%q, %v), want (%q, nil)", data, err, "blob")
	}

	if err := fs.Delete(ctx, user); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := fs.Exists(ctx, user); ok {
		t.Error("Exists after delete = true, want false")
	}
	if err := fs.Delete(ctx, user); err != nil {
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

	if err := store.Session(user).StoreSession(ctx, []byte("plaintext")); err != nil {
		t.Fatalf("StoreSession: %v", err)
	}

	// The backend must hold ciphertext, not the plaintext.
	raw, err := backend.Session(user).LoadSession(ctx)
	if err != nil {
		t.Fatalf("backend LoadSession: %v", err)
	}
	if string(raw) == "plaintext" {
		t.Fatal("backend stores plaintext; encryption wrapper is not applied")
	}

	got, err := store.Session(user).LoadSession(ctx)
	if err != nil || string(got) != "plaintext" {
		t.Errorf("LoadSession = (%q, %v), want (%q, nil)", got, err, "plaintext")
	}

	// A blob that cannot be decrypted must surface as ErrCorruptSession —
	// distinct from ErrNotFound so the caller does not mistake a key/issuer
	// misconfiguration for "new user" and destroy a recoverable session.
	if err := backend.Session(user).StoreSession(ctx, []byte("garbage")); err != nil {
		t.Fatalf("backend StoreSession: %v", err)
	}
	_, err = store.Session(user).LoadSession(ctx)
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
	if _, err := store.Session(1).LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("LoadSession on empty encrypted store: err = %v, want session.ErrNotFound", err)
	}
}
