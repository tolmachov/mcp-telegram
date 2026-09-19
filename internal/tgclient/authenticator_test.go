package tgclient

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserAuthenticatorReadsSequentialPipedAnswers pins that the login code and
// the 2FA password can both arrive in one non-terminal stream: a per-prompt
// buffered reader would consume the password along with the code and then hit
// EOF on the second prompt.
func TestUserAuthenticatorReadsSequentialPipedAnswers(t *testing.T) {
	var out bytes.Buffer
	a := newUserAuthenticator("+10000000000", strings.NewReader("12345\nsecret pass\n"), &out)

	code, err := a.Code(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, "12345", code)

	password, err := a.Password(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "secret pass", password)

	assert.Equal(t, "Enter login code: Enter 2FA password: ", out.String())
}
