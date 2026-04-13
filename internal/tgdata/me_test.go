package tgdata

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestUserInfoZeroValueID confirms the zero-value invariant relied on by
// GetCurrentUser: info.ID == 0 is the sentinel that no self-record was found.
// GetCurrentUser itself requires a live *tg.Client and is covered by integration
// tests; this unit test locks in the sentinel value so refactors don't break
// the guard at me.go:31.
func TestUserInfoZeroValueID(t *testing.T) {
	info := UserInfo{}
	assert.Equal(t, int64(0), info.ID)
}
