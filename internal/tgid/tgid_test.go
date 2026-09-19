package tgid

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserIDRoundTripAndValidation(t *testing.T) {
	id, err := Parse("123456")
	require.NoError(t, err)
	assert.Equal(t, int64(123456), id.Int64())
	assert.Equal(t, "123456", id.String())
	for _, input := range []string{"", "not-a-number", "0", "-1"} {
		_, err := Parse(input)
		assert.Error(t, err, input)
	}
}
