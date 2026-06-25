package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tolmachov/mcp-telegram/internal/flags"
)

func TestResolveKeyKnownAliases(t *testing.T) {
	cases := []struct {
		alias     string
		canonical string
	}{
		{"api-id", flags.EnvTelegramAPIID},
		{"api-hash", flags.EnvTelegramAPIHash},
		{"anthropic", flags.EnvAnthropicAPIKey},
		{"gemini", flags.EnvGeminiAPIKey},
	}
	for _, tc := range cases {
		t.Run(tc.alias, func(t *testing.T) {
			got, err := ResolveKey(tc.alias)
			require.NoError(t, err)
			assert.Equal(t, tc.canonical, got)
		})
	}
}

func TestResolveKeyIsCaseInsensitive(t *testing.T) {
	got, err := ResolveKey("API-ID")
	require.NoError(t, err)
	assert.Equal(t, flags.EnvTelegramAPIID, got)

	got, err = ResolveKey("Api-Hash")
	require.NoError(t, err)
	assert.Equal(t, flags.EnvTelegramAPIHash, got)
}

func TestResolveKeyUnknownAliasLists(t *testing.T) {
	_, err := ResolveKey("garbage")
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, `"garbage"`)
	// Every known alias must appear in the error so the user has a crib.
	for _, alias := range []string{"api-id", "api-hash", "anthropic", "gemini"} {
		assert.Contains(t, msg, alias)
	}
}

func TestResolveKeyRejectsCanonicalName(t *testing.T) {
	// The user-facing CLI must not accept raw env-var names — only aliases.
	_, err := ResolveKey(flags.EnvTelegramAPIID)
	require.Error(t, err)
}

func TestAliasForRoundTrip(t *testing.T) {
	for _, alias := range []string{"api-id", "api-hash", "anthropic", "gemini"} {
		canonical, err := ResolveKey(alias)
		require.NoError(t, err)
		assert.Equal(t, alias, AliasFor(canonical), "round-trip for %q", alias)
	}
}

func TestAliasForUnknownCanonicalPassesThrough(t *testing.T) {
	// AliasFor falls back to the canonical name for unregistered keys so
	// List() never drops entries it can't describe.
	assert.Equal(t, "UNREGISTERED_VAR", AliasFor("UNREGISTERED_VAR"))
}

func TestAliasListIsSorted(t *testing.T) {
	// The ordering is part of the user-visible error message; verify it
	// really is sorted so the message is stable across Go's map-iteration
	// randomization.
	got := aliasList()
	assert.True(t, len(got) >= 2, "need at least two aliases to verify ordering")
	sorted := make([]string, len(got))
	copy(sorted, got)
	for i := 1; i < len(sorted); i++ {
		assert.LessOrEqual(t, strings.Compare(sorted[i-1], sorted[i]), 0, "aliasList is not sorted at index %d", i)
	}
}
