package authsrv

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/tolmachov/mcp-telegram/internal/keyring"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

func testKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, keyring.MasterKeyLen)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(b)
}

// testRing parses encoded master keys, or one fresh random key if none.
func testRing(t *testing.T, keys ...string) *keyring.Ring {
	t.Helper()
	if len(keys) == 0 {
		keys = []string{testKey(t)}
	}
	ring, err := keyring.Parse(keys)
	require.NoError(t, err)
	return ring
}

func testSealer(t *testing.T, keys ...string) *sealer {
	t.Helper()
	if len(keys) == 0 {
		keys = []string{testKey(t)}
	}
	ring, err := newKeyRing(testRing(t, keys...))
	require.NoError(t, err)
	return newSealer(ring, testIssuer)
}

// testGrant returns grant claims valid for testIssuer.
func testGrant() grantClaims {
	return grantClaims{
		Resource: testIssuer, SessionID: sessionstore.NewSID(),
		SessionKey: sessionstore.NewSessionKey(), Family: sessionstore.NewSID(),
	}
}

// TestSealerOpensGoldenBlobs pins the token formats (key ID, nonce, HKDF
// labels, AAD, MAC input, claims JSON): artifacts issued by an earlier build
// under fixed keys must still decrypt and verify, so deployed tokens survive
// refactors. The golden access token's claims are not valid for any issuer, so
// it goes through the format layer, not openBlob.
func TestSealerOpensGoldenBlobs(t *testing.T) {
	primary := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x22}, keyring.MasterKeyLen))
	old := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x11}, keyring.MasterKeyLen))
	s := testSealer(t, primary, old)

	const access = "mcp_at_nyyIjJlooCsBp3QvMobM_0C5Vt8r4QfK1abHM7NMX58iSaVsMYOFbfUj_JDbRllwWC63kCahu_lNkNSiZdkQ-87p2_6gg0ibhOIUuSNj5poo9Rrk3HSKQp-koyRWiIyesPatjVTPcA"
	ac, err := decryptBlob(s, accessBlob, access)
	require.NoError(t, err)
	assert.Equal(t, accessClaims{Subject: 123456, ClientID: "cid", grantClaims: grantClaims{Family: "fam"}, IssuedAt: 1700000000, ExpiresAt: 1700003600}, ac)

	const clientID = "mcp_cid_eyJydSI6WyJodHRwOi8vMTI3LjAuMC4xL2NiIl0sImlhdCI6MTcwMDAwMDAwMH0.etkRJef0si75Lq9N1H3Mhqibiw15aAGtTnRLWpTAQK8"
	var cc clientIDClaims
	require.NoError(t, s.verifyClientID(clientID, &cc))
	assert.Equal(t, []string{"http://127.0.0.1/cb"}, cc.RedirectURIs)
}

func TestSealOpenRoundtrip(t *testing.T) {
	s := testSealer(t)
	now := time.Now()
	in := accessClaims{Subject: 123456, Username: "durov", ClientID: "cid", grantClaims: testGrant(), ExpiresAt: 42, IssuedAt: now.Unix()}
	blob, err := sealBlob(s, accessBlob, in)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(blob, prefixAccess))

	out, err := openBlob(s, accessBlob, blob, now)
	require.NoError(t, err)
	assert.Equal(t, in, out)
}

func TestOpenRejects(t *testing.T) {
	s := testSealer(t, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0}, keyring.MasterKeyLen)))
	now := time.Now()
	blob, err := sealBlob(s, accessBlob, accessClaims{Subject: 1, grantClaims: testGrant(), IssuedAt: now.Unix()})
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
	t.Run("invalid claims", func(t *testing.T) {
		invalid, err := encryptBlob(s, accessBlob, accessClaims{Subject: 1, IssuedAt: now.Unix()})
		require.NoError(t, err)
		_, err = openBlob(s, accessBlob, invalid, now)
		assert.ErrorIs(t, err, errInvalidClaims)
		assert.ErrorIs(t, err, errInvalidBlob)
	})
	t.Run("wrong issuer", func(t *testing.T) {
		other := newSealer(s.ring, "https://other.example")
		_, err := openBlob(other, accessBlob, blob, now)
		assert.ErrorIs(t, err, errInvalidBlob)
	})
	t.Run("wrong key reports unknown key id", func(t *testing.T) {
		other := testSealer(t, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, keyring.MasterKeyLen)))
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
		stale, err := sealBlob(s, codeBlob, codeClaims{Subject: 1, grantClaims: testGrant(), IssuedAt: now.Add(-2 * codeTTL).Unix()})
		require.NoError(t, err)
		_, err = openBlob(s, codeBlob, stale, now)
		assert.ErrorIs(t, err, errBlobExpired)
		assert.ErrorIs(t, err, errInvalidBlob)
	})
	t.Run("future issued-at beyond clock skew rejected", func(t *testing.T) {
		future, err := sealBlob(s, accessBlob, accessClaims{Subject: 1, grantClaims: testGrant(), IssuedAt: now.Add(maxIssueSkew + time.Second).Unix()})
		require.NoError(t, err)
		_, err = openBlob(s, accessBlob, future, now)
		assert.ErrorIs(t, err, errIssuedInFuture)
		assert.ErrorIs(t, err, errInvalidBlob)
	})
}

func TestKeyRotation(t *testing.T) {
	// Fixed keys have different one-byte IDs; random pairs collide 1/256 of the time.
	oldKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0}, keyring.MasterKeyLen))
	newKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, keyring.MasterKeyLen))
	now := time.Now()

	oldSealer := testSealer(t, oldKey)
	blob, err := sealBlob(oldSealer, refreshBlob, refreshClaims{Subject: 42, grantClaims: testGrant(), IssuedAt: now.Unix(), LoginAt: now.Unix()})
	require.NoError(t, err)
	signed, err := oldSealer.signClientID(clientIDClaims{RedirectURIs: []string{"http://localhost/cb"}})
	require.NoError(t, err)

	// Rotated ring: new key seals, old key still opens and verifies.
	rotated := testSealer(t, newKey, oldKey)
	rc, err := openBlob(rotated, refreshBlob, blob, now)
	require.NoError(t, err)
	assert.Equal(t, tgid.UserID(42), rc.Subject)
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

// TestEverySpecChecksItsClaims pins that each blob kind's own rules hold on
// open, including the ones no handler checked before: the code's subject and
// the refresh token's generation and login time.
func TestEverySpecChecksItsClaims(t *testing.T) {
	s := testSealer(t)
	now := time.Now()
	iat := now.Unix()
	open := map[string]func() error{
		"code without subject": func() error {
			return openInvalid(s, codeBlob, codeClaims{grantClaims: testGrant(), IssuedAt: iat}, now)
		},
		"code with malformed grant": func() error {
			return openInvalid(s, codeBlob, codeClaims{Subject: 1, grantClaims: grantClaims{Resource: testIssuer}, IssuedAt: iat}, now)
		},
		"refresh with negative generation": func() error {
			return openInvalid(s, refreshBlob, refreshClaims{Subject: 1, grantClaims: testGrant(), Generation: -1, IssuedAt: iat, LoginAt: iat}, now)
		},
		"refresh without login time": func() error {
			return openInvalid(s, refreshBlob, refreshClaims{Subject: 1, grantClaims: testGrant(), IssuedAt: iat}, now)
		},
		"refresh logged in after issue": func() error {
			return openInvalid(s, refreshBlob, refreshClaims{Subject: 1, grantClaims: testGrant(), IssuedAt: iat, LoginAt: iat + 1}, now)
		},
		"state for another resource": func() error {
			return openInvalid(s, stateBlob, stateClaims{Resource: "https://other.example", IssuedAt: iat}, now)
		},
	}
	for name, run := range open {
		assert.ErrorIs(t, run(), errInvalidClaims, name)
	}
}

// openInvalid seals v without checking it and returns the error of opening it.
func openInvalid[T claims](s *sealer, spec blobSpec[T], v T, now time.Time) error {
	blob, err := encryptBlob(s, spec, v)
	if err != nil {
		return err
	}
	_, err = openBlob(s, spec, blob, now)
	return err
}

// TestSealOpenProperty pins that the format layer round-trips any claims.
func TestSealOpenProperty(t *testing.T) {
	s := testSealer(t)
	rapid.Check(t, func(t *rapid.T) {
		in := codeClaims{
			Subject:       tgid.UserID(rapid.Int64().Draw(t, "sub")),
			Username:      rapid.String().Draw(t, "un"),
			ClientID:      rapid.String().Draw(t, "cid"),
			RedirectURI:   rapid.String().Draw(t, "ru"),
			CodeChallenge: rapid.String().Draw(t, "cc"),
			grantClaims:   grantClaims{Resource: rapid.String().Draw(t, "res")},
			IssuedAt:      rapid.Int64().Draw(t, "iat"),
		}
		blob, err := encryptBlob(s, codeBlob, in)
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		out, err := decryptBlob(s, codeBlob, blob)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		assert.Equal(t, in, out)
	})
}
