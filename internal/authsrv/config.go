// Package authsrv implements the embedded OAuth 2.1 authorization server that
// protects the MCP HTTP transport. The server plays both RFC 9728 roles at
// once: it is the MCP protected resource and the authorization server that
// MCP clients discover, register against (RFC 7591), and obtain tokens from.
// Telegram itself is the identity provider: instead of redirecting to an
// upstream IdP, /authorize renders a QR code that the user scans from the
// Telegram app (Settings → Devices → Link Desktop Device), which both proves
// who they are and produces the MTProto session the server needs to act on
// their behalf. Every artifact the server issues (authorization code, access
// token, refresh token, OAuth state, client ID) is a self-contained sealed
// blob, so no server-side session or token storage is required beyond the
// Telegram session store itself.
package authsrv

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// defaultExtraRedirects are HTTPS redirect URIs accepted in addition to
// loopback URIs: the claude.ai / Claude Desktop connector callbacks.
var defaultExtraRedirects = []string{
	"https://claude.ai/api/mcp/auth_callback",
	"https://claude.com/api/mcp/auth_callback",
}

// defaultRefreshTokenTTL bounds how long a refresh token may be used before
// the user must log in again.
const defaultRefreshTokenTTL = 30 * 24 * time.Hour

// wildcardUser is the sentinel flag entry that opens the login gate to any
// Telegram account. It cannot be combined with specific user IDs.
const wildcardUser = "*"

// Allowlist is the login gate. It is EITHER a wildcard (any Telegram account
// may log in) OR an explicit set of allowed user IDs — never both. The two
// states are mutually exclusive by construction, so the previously unsafe
// "wildcard plus a specific list" combination (which silently ignored the list
// and opened the gate to everyone) is unrepresentable. The zero value is an
// empty explicit list, which fails Validate — an unconfigured gate never
// silently allows anyone.
type Allowlist struct {
	all bool
	ids []tgid.UserID
}

// AllowAll returns an Allowlist that admits any Telegram account. The
// deployment is then only as private as its URL.
func AllowAll() Allowlist { return Allowlist{all: true} }

// AllowUsers returns an Allowlist restricted to the given Telegram user IDs.
func AllowUsers(ids ...tgid.UserID) Allowlist { return Allowlist{ids: ids} }

// ParseAllowlist builds an Allowlist from raw --auth-allowed-users entries.
// A single "*" means wildcard; every other entry must be a positive Telegram
// user ID. Blank entries are ignored. Mixing "*" with explicit IDs is rejected
// so an operator who expects the list to still restrict access cannot silently
// end up wide open. An all-blank/empty input yields an empty (invalid) list;
// the "at least one entry" requirement is reported by Config.Validate.
func ParseAllowlist(entries []string) (Allowlist, error) {
	var ids []tgid.UserID
	wildcard := false
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		switch entry {
		case "":
			continue
		case wildcardUser:
			wildcard = true
		default:
			id, err := tgid.Parse(entry)
			if err != nil {
				return Allowlist{}, fmt.Errorf("invalid allowed user %q: %w", entry, err)
			}
			ids = append(ids, id)
		}
	}
	if wildcard && len(ids) > 0 {
		return Allowlist{}, fmt.Errorf("%q (allow all) cannot be combined with specific user IDs", wildcardUser)
	}
	if wildcard {
		return AllowAll(), nil
	}
	return Allowlist{ids: ids}, nil
}

// IsWildcard reports whether the gate admits any account.
func (a Allowlist) IsWildcard() bool { return a.all }

// UserIDs returns a copy of the explicit allowlist (empty for a wildcard).
func (a Allowlist) UserIDs() []tgid.UserID { return slices.Clone(a.ids) }

// configured reports whether the gate admits anyone at all — true for a
// wildcard or a non-empty list, false for the zero value.
func (a Allowlist) configured() bool { return a.all || len(a.ids) > 0 }

// allows reports whether id may log in and use tokens.
func (a Allowlist) allows(id tgid.UserID) bool {
	return a.all || slices.Contains(a.ids, id)
}

// Config holds the settings of the embedded authorization server.
type Config struct {
	// IssuerURL is the public base URL of this service (no trailing slash),
	// e.g. "https://mcp-telegram.example.com". It is the OAuth issuer, the
	// MCP resource identifier, and the AAD binding of all sealed blobs.
	IssuerURL string
	// Allow is the login gate: either specific Telegram accounts (AllowUsers)
	// or any account (AllowAll). Required — an unconfigured (zero) Allow fails
	// Validate, so the server never starts wide open by accident.
	Allow Allowlist
	// TokenKeys are base64-encoded 32-byte master keys. The first key seals
	// new blobs; all keys can open existing ones, enabling rotation without
	// a mass logout.
	TokenKeys []string
	// ExtraRedirects are additional exact-match redirect URIs accepted at
	// registration on top of the defaults (claude.ai/claude.com callbacks)
	// and the always-allowed loopback URIs.
	ExtraRedirects []string
	// RefreshTokenTTL caps refresh-token lifetime. Zero means the default
	// (30 days).
	RefreshTokenTTL time.Duration
}

// Validate checks the configuration and normalizes IssuerURL.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("auth config must not be nil")
	}
	c.IssuerURL = strings.TrimRight(c.IssuerURL, "/")
	u, err := url.Parse(c.IssuerURL)
	if err != nil {
		return fmt.Errorf("invalid issuer URL %q: %w", c.IssuerURL, err)
	}
	switch {
	case u.Scheme == "https" && u.Host != "":
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
		// http is acceptable only for local development.
	default:
		return fmt.Errorf("issuer URL %q must be https (or http on localhost)", c.IssuerURL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("issuer URL %q must not contain a query or fragment", c.IssuerURL)
	}
	if !c.Allow.configured() {
		return fmt.Errorf("at least one allowed Telegram user ID is required (or %q to allow all)", wildcardUser)
	}
	for _, id := range c.Allow.ids {
		if id <= 0 {
			return fmt.Errorf("allowed user IDs must be positive, got %d", id)
		}
	}
	if len(c.TokenKeys) == 0 {
		return fmt.Errorf("at least one token key is required")
	}
	if _, err := newKeyRing(c.TokenKeys); err != nil {
		return fmt.Errorf("invalid token keys: %w", err)
	}
	for _, r := range c.ExtraRedirects {
		if _, err := url.Parse(r); err != nil {
			return fmt.Errorf("invalid extra redirect URI %q: %w", r, err)
		}
	}
	if c.RefreshTokenTTL < 0 {
		return fmt.Errorf("refresh token TTL must not be negative")
	}
	return nil
}

// userAllowed reports whether a Telegram account may log in and use tokens.
// It is enforced at three points: when the QR login completes, on every
// refresh grant, and on every access-token verification — so removing an ID
// from the allowlist (and redeploying) cuts off already-issued tokens too.
// With a wildcard gate every account passes.
func (c *Config) userAllowed(id tgid.UserID) bool {
	return c.Allow.allows(id)
}

// refreshTokenTTL returns the configured refresh-token TTL or the default.
func (c *Config) refreshTokenTTL() time.Duration {
	if c.RefreshTokenTTL > 0 {
		return c.RefreshTokenTTL
	}
	return defaultRefreshTokenTTL
}
