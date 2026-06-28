package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInputSchemaWithEnums(t *testing.T) {
	schema := inputSchemaWithEnums[SummarizeChatInput](map[string][]any{
		"period": {"day", "week", "month"},
	})

	require.NotNil(t, schema.Properties["period"])
	assert.Equal(t, []any{"day", "week", "month"}, schema.Properties["period"].Enum)

	// Non-enum properties are left untouched (description preserved, no enum).
	require.NotNil(t, schema.Properties["chat_id"])
	assert.Nil(t, schema.Properties["chat_id"].Enum)
}

func TestInputSchemaWithEnumsPanicsOnUnknownProperty(t *testing.T) {
	assert.Panics(t, func() {
		inputSchemaWithEnums[SummarizeChatInput](map[string][]any{
			"does_not_exist": {"x"},
		})
	})
}
