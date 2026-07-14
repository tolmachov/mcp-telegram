package authsrv

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

func validConfig(t *testing.T) *Config {
	t.Helper()
	return &Config{
		IssuerURL: "https://mcp.example.com",
		Allow:     AllowUsers(123456),
		TokenKeys: []string{testKey(t)},
	}
}

func TestConfigAllowAll(t *testing.T) {
	c := &Config{
		IssuerURL: "https://mcp.example.com",
		Allow:     AllowAll(),
		TokenKeys: []string{testKey(t)},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate with a wildcard allowlist should pass: %v", err)
	}
	if !c.userAllowed(1) || !c.userAllowed(999999) {
		t.Error("userAllowed should return true for any id with a wildcard gate")
	}
}

func TestParseAllowlist(t *testing.T) {
	t.Run("specific ids", func(t *testing.T) {
		a, err := ParseAllowlist([]string{"123", "  456  "})
		require.NoError(t, err)
		assert.False(t, a.IsWildcard())
		assert.Equal(t, []tgid.UserID{123, 456}, a.UserIDs())
	})
	t.Run("wildcard alone", func(t *testing.T) {
		a, err := ParseAllowlist([]string{"*"})
		require.NoError(t, err)
		assert.True(t, a.IsWildcard())
		assert.Empty(t, a.UserIDs())
	})
	t.Run("blanks ignored", func(t *testing.T) {
		a, err := ParseAllowlist([]string{"", "123", "  "})
		require.NoError(t, err)
		assert.Equal(t, []tgid.UserID{123}, a.UserIDs())
	})
	t.Run("wildcard mixed with id rejected", func(t *testing.T) {
		_, err := ParseAllowlist([]string{"*", "123"})
		require.Error(t, err)
		assert.ErrorContains(t, err, "cannot be combined")
	})
	t.Run("non-numeric rejected", func(t *testing.T) {
		_, err := ParseAllowlist([]string{"nope"})
		assert.Error(t, err)
	})
	t.Run("empty yields unconfigured gate", func(t *testing.T) {
		a, err := ParseAllowlist(nil)
		require.NoError(t, err)
		assert.False(t, a.configured())
	})
}

func TestConfigValidate(t *testing.T) {
	t.Run("valid https config passes", func(t *testing.T) {
		assert.NoError(t, validConfig(t).Validate())
	})
	t.Run("trailing slash is normalized", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.IssuerURL = "https://mcp.example.com/"
		require.NoError(t, cfg.Validate())
		assert.Equal(t, "https://mcp.example.com", cfg.IssuerURL)
	})
	t.Run("http localhost allowed for development", func(t *testing.T) {
		cfg := validConfig(t)
		cfg.IssuerURL = "http://localhost:8080"
		assert.NoError(t, cfg.Validate())
	})

	fail := func(name string, mutate func(*Config), wantSubstr string) {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig(t)
			mutate(cfg)
			err := cfg.Validate()
			require.Error(t, err)
			assert.ErrorContains(t, err, wantSubstr)
		})
	}

	fail("http non-loopback issuer", func(c *Config) { c.IssuerURL = "http://intranet.example" }, "must be https")
	fail("issuer with query", func(c *Config) { c.IssuerURL = "https://mcp.example.com?x=1" }, "query or fragment")
	fail("issuer with fragment", func(c *Config) { c.IssuerURL = "https://mcp.example.com#frag" }, "query or fragment")
	fail("empty allowlist", func(c *Config) { c.Allow = Allowlist{} }, "allowed Telegram user ID")
	fail("non-positive user id", func(c *Config) { c.Allow = AllowUsers(0) }, "must be positive")
	fail("negative user id", func(c *Config) { c.Allow = AllowUsers(-5) }, "must be positive")
	fail("no token keys", func(c *Config) { c.TokenKeys = nil }, "token key")
	fail("bad token key", func(c *Config) { c.TokenKeys = []string{"nope"} }, "invalid token keys")
	fail("negative refresh TTL", func(c *Config) { c.RefreshTokenTTL = -time.Hour }, "must not be negative")
}

func TestNewCopiesConfig(t *testing.T) {
	cfg := validConfig(t)
	a, err := New(cfg, nil, sessionstore.NewMemory(), neverStartLogin)
	require.NoError(t, err)
	t.Cleanup(a.Close)

	// Mutating the caller's config after construction must not affect the
	// server (the sealer AAD and metadata must stay in sync).
	cfg.IssuerURL = "https://hijacked.example"
	assert.Equal(t, "https://mcp.example.com", a.cfg.IssuerURL)
}

func TestNewRequiresCollaborators(t *testing.T) {
	_, err := New(validConfig(t), nil, nil, neverStartLogin)
	assert.ErrorContains(t, err, "session store")

	_, err = New(validConfig(t), nil, sessionstore.NewMemory(), nil)
	assert.ErrorContains(t, err, "start-login")
}

func TestUserAllowed(t *testing.T) {
	cfg := &Config{Allow: AllowUsers(111, 222)}
	assert.True(t, cfg.userAllowed(111))
	assert.True(t, cfg.userAllowed(222))
	assert.False(t, cfg.userAllowed(333))
	assert.False(t, cfg.userAllowed(0))
}
