package keyring

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func randomKey(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, MasterKeyLen)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

func TestParse(t *testing.T) {
	t.Run("rejects empty", func(t *testing.T) {
		_, err := Parse(nil)
		assert.Error(t, err)
	})
	t.Run("rejects non-base64", func(t *testing.T) {
		_, err := Parse([]string{"not base64 at all!!"})
		assert.Error(t, err)
	})
	t.Run("rejects wrong length", func(t *testing.T) {
		_, err := Parse([]string{base64.StdEncoding.EncodeToString([]byte("short"))})
		assert.Error(t, err)
	})
	t.Run("rejects key-ID collision", func(t *testing.T) {
		k := base64.StdEncoding.EncodeToString(randomKey(t))
		_, err := Parse([]string{k, k})
		assert.ErrorContains(t, err, "collide")
	})
	t.Run("accepts every base64 encoding", func(t *testing.T) {
		b := randomKey(t)
		for _, enc := range []*base64.Encoding{
			base64.StdEncoding, base64.RawStdEncoding,
			base64.URLEncoding, base64.RawURLEncoding,
		} {
			ring, err := Parse([]string{enc.EncodeToString(b)})
			require.NoError(t, err)
			assert.Equal(t, b, ring.Primary().Master)
		}
	})
}

// TestKeyIDs pins the key ID (first byte of the master key's SHA-256): it is
// embedded in every sealed token and session blob, so changing it would strand
// them all.
func TestKeyIDs(t *testing.T) {
	primary := bytes.Repeat([]byte{0x22}, MasterKeyLen)
	old := bytes.Repeat([]byte{0x11}, MasterKeyLen)
	ring, err := Parse([]string{
		base64.StdEncoding.EncodeToString(primary),
		base64.StdEncoding.EncodeToString(old),
	})
	require.NoError(t, err)
	assert.Equal(t, byte(0x9f), ring.Primary().ID)
	assert.Equal(t, primary, ring.Primary().Master)
	k, ok := ring.ByID(0x02)
	require.True(t, ok)
	assert.Equal(t, old, k.Master)
	_, ok = ring.ByID(0x00)
	assert.False(t, ok)
	var ids []byte
	for _, k := range ring.Keys() {
		ids = append(ids, k.ID)
	}
	assert.Equal(t, []byte{0x9f, 0x02}, ids, "Keys yields the sealing key first")
}
