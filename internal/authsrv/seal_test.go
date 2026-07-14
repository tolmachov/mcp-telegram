package authsrv

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func testKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, masterKeyLen)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(b)
}

func testSealer(t *testing.T, keys ...string) *sealer {
	t.Helper()
	if len(keys) == 0 {
		keys = []string{testKey(t)}
	}
	ring, err := newKeyRing(keys)
	require.NoError(t, err)
	return newSealer(ring, "https://issuer.example")
}

func TestNewKeyRing(t *testing.T) {
	t.Run("rejects empty", func(t *testing.T) {
		_, err := newKeyRing(nil)
		assert.Error(t, err)
	})
	t.Run("rejects non-base64", func(t *testing.T) {
		_, err := newKeyRing([]string{"not base64 at all!!"})
		assert.Error(t, err)
	})
	t.Run("rejects wrong length", func(t *testing.T) {
		_, err := newKeyRing([]string{base64.StdEncoding.EncodeToString([]byte("short"))})
		assert.Error(t, err)
	})
	t.Run("rejects duplicate key", func(t *testing.T) {
		k := testKey(t)
		_, err := newKeyRing([]string{k, k})
		assert.ErrorContains(t, err, "collide")
	})
	t.Run("accepts url-safe raw base64", func(t *testing.T) {
		b := make([]byte, masterKeyLen)
		_, err := rand.Read(b)
		require.NoError(t, err)
		_, err = newKeyRing([]string{base64.RawURLEncoding.EncodeToString(b)})
		assert.NoError(t, err)
	})
}

func TestSealOpenRoundtrip(t *testing.T) {
	s := testSealer(t)
	now := time.Now()
	in := accessClaims{Subject: "123456", Username: "durov", ClientID: "cid", ExpiresAt: 42, IssuedAt: now.Unix()}
	blob, err := sealBlob(s, accessBlob, in)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(blob, prefixAccess))

	out, err := openBlob(s, accessBlob, blob, now)
	require.NoError(t, err)
	assert.Equal(t, in, out)
}

func TestOpenRejects(t *testing.T) {
	s := testSealer(t)
	now := time.Now()
	blob, err := sealBlob(s, accessBlob, accessClaims{Subject: "1", IssuedAt: now.Unix()})
	require.NoError(t, err)

	t.Run("wrong spec (prefix mismatch)", func(t *testing.T) {
		_, err := openBlob(s, refreshBlob, blob, now)
		assert.ErrorIs(t, err, errInvalidBlob)
	})
	t.Run("wrong kind under stolen prefix", func(t *testing.T) {
		raw := strings.TrimPrefix(blob, prefixAccess)
		_, err := openBlob(s, refreshBlob, prefixRefresh+raw, now)
		assert.ErrorIs(t, err, errInvalidBlob)
	})
	t.Run("wrong issuer", func(t *testing.T) {
		other := newSealer(s.ring, "https://other.example")
		_, err := openBlob(other, accessBlob, blob, now)
		assert.ErrorIs(t, err, errInvalidBlob)
	})
	t.Run("wrong key reports unknown key id", func(t *testing.T) {
		other := testSealer(t)
		_, err := openBlob(other, accessBlob, blob, now)
		assert.ErrorIs(t, err, errUnknownKeyID)
		assert.ErrorIs(t, err, errInvalidBlob)
	})
	t.Run("tampered ciphertext", func(t *testing.T) {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(blob, prefixAccess))
		require.NoError(t, err)
		raw[len(raw)-1] ^= 0x01
		tampered := prefixAccess + base64.RawURLEncoding.EncodeToString(raw)
		_, err = openBlob(s, accessBlob, tampered, now)
		assert.ErrorIs(t, err, errInvalidBlob)
	})
	t.Run("garbage", func(t *testing.T) {
		for _, g := range []string{prefixAccess + "@@@", prefixAccess, ""} {
			_, err := openBlob(s, accessBlob, g, now)
			assert.ErrorIs(t, err, errInvalidBlob)
		}
	})
	t.Run("expired blob rejected by spec TTL", func(t *testing.T) {
		stale, err := sealBlob(s, codeBlob, codeClaims{Subject: "1", IssuedAt: now.Add(-2 * codeTTL).Unix()})
		require.NoError(t, err)
		_, err = openBlob(s, codeBlob, stale, now)
		assert.ErrorIs(t, err, errBlobExpired)
		assert.ErrorIs(t, err, errInvalidBlob)
	})
}

func TestKeyRotation(t *testing.T) {
	oldKey, newKey := testKey(t), testKey(t)
	now := time.Now()

	oldSealer := testSealer(t, oldKey)
	blob, err := sealBlob(oldSealer, refreshBlob, refreshClaims{Subject: "42", IssuedAt: now.Unix(), LoginAt: now.Unix()})
	require.NoError(t, err)
	signed, err := oldSealer.signClientID(clientIDClaims{RedirectURIs: []string{"http://localhost/cb"}})
	require.NoError(t, err)

	// Rotated ring: new key seals, old key still opens and verifies.
	rotated := testSealer(t, newKey, oldKey)
	rc, err := openBlob(rotated, refreshBlob, blob, now)
	require.NoError(t, err)
	assert.Equal(t, "42", rc.Subject)
	var cc clientIDClaims
	require.NoError(t, rotated.verifyClientID(signed, &cc))

	fresh, err := sealBlob(rotated, refreshBlob, rc)
	require.NoError(t, err)
	// Old-only ring cannot open blobs sealed by the new key, and the reason
	// is the operationally-distinguishable "unknown key id".
	_, err = openBlob(oldSealer, refreshBlob, fresh, now)
	assert.ErrorIs(t, err, errUnknownKeyID)
}

func TestSignVerify(t *testing.T) {
	s := testSealer(t)
	in := clientIDClaims{RedirectURIs: []string{"https://claude.ai/api/mcp/auth_callback"}, ClientName: "Claude"}
	id, err := s.signClientID(in)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(id, prefixClientID))

	var out clientIDClaims
	require.NoError(t, s.verifyClientID(id, &out))
	assert.Equal(t, in, out)

	t.Run("tampered payload keeps mac invalid", func(t *testing.T) {
		forged := clientIDClaims{RedirectURIs: []string{"https://evil.example/cb"}}
		forgedID, err := s.signClientID(forged)
		require.NoError(t, err)
		// Splice forged payload with the legitimate mac.
		_, legitMac, _ := strings.Cut(strings.TrimPrefix(id, prefixClientID), ".")
		forgedPayload, _, _ := strings.Cut(strings.TrimPrefix(forgedID, prefixClientID), ".")
		spliced := prefixClientID + forgedPayload + "." + legitMac
		assert.ErrorIs(t, s.verifyClientID(spliced, &out), errInvalidBlob)
	})
	t.Run("missing dot", func(t *testing.T) {
		assert.ErrorIs(t, s.verifyClientID(prefixClientID+"abc", &out), errInvalidBlob)
	})
}

func TestExpired(t *testing.T) {
	now := time.Now()
	assert.False(t, expired(now.Unix(), time.Minute, now))
	assert.False(t, expired(now.Unix(), time.Minute, now.Add(59*time.Second)))
	assert.True(t, expired(now.Unix(), time.Minute, now.Add(61*time.Second)))
	assert.True(t, expired(0, time.Minute, now))
}

func TestSealOpenProperty(t *testing.T) {
	s := testSealer(t)
	now := time.Now()
	rapid.Check(t, func(t *rapid.T) {
		in := codeClaims{
			Subject:       rapid.String().Draw(t, "sub"),
			Username:      rapid.String().Draw(t, "un"),
			ClientID:      rapid.String().Draw(t, "cid"),
			RedirectURI:   rapid.String().Draw(t, "ru"),
			CodeChallenge: rapid.String().Draw(t, "cc"),
			Resource:      rapid.String().Draw(t, "res"),
			// Within the code TTL so openBlob's expiry enforcement passes.
			IssuedAt: rapid.Int64Range(now.Add(-codeTTL/2).Unix(), now.Unix()).Draw(t, "iat"),
		}
		blob, err := sealBlob(s, codeBlob, in)
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		out, err := openBlob(s, codeBlob, blob, now)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		assert.Equal(t, in, out)
	})
}
