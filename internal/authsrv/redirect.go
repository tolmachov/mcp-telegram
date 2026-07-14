package authsrv

import (
	"net"
	"net/url"
	"slices"
	"strings"
)

// redirectPolicy decides which redirect URIs a client may register and use.
// Three tiers (RFC 8252 §7.3 for the first):
//  1. Loopback HTTP on any port/path — native clients (Claude Code, Cursor,
//     MCP inspector) bind an ephemeral localhost port per flow.
//  2. Exact-match HTTPS entries — hosted clients (claude.ai / Claude Desktop
//     connector callbacks by default, extendable via config).
//  3. Custom schemes (e.g. cursor://...) — only if explicitly listed.
type redirectPolicy struct {
	exact []string
}

// newRedirectPolicy builds the policy from the built-in defaults plus
// configured extras (exact HTTPS or custom-scheme URIs).
func newRedirectPolicy(extra []string) *redirectPolicy {
	return &redirectPolicy{exact: slices.Concat(defaultExtraRedirects, extra)}
}

// allowed reports whether a single redirect URI is acceptable.
func (p *redirectPolicy) allowed(raw string) bool {
	if slices.Contains(p.exact, raw) {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == "http" && isLoopbackHost(u.Hostname()) && u.Fragment == ""
}

// isLoopbackHost reports whether host is localhost or a loopback IP literal.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// matchRegistered reports whether the redirect_uri presented at /authorize
// matches one of the URIs registered for the client. Comparison is exact
// (OAuth 2.1), with one carve-out required by RFC 8252 §7.3: for loopback
// HTTP URIs the port may differ from the registered one, because native
// clients bind a fresh ephemeral port for every flow.
func matchRegistered(registered []string, raw string) bool {
	if slices.Contains(registered, raw) {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || !isLoopbackHost(u.Hostname()) {
		return false
	}
	for _, r := range registered {
		ru, err := url.Parse(r)
		if err != nil || ru.Scheme != "http" || !isLoopbackHost(ru.Hostname()) {
			continue
		}
		if strings.EqualFold(ru.Hostname(), u.Hostname()) && ru.Path == u.Path {
			return true
		}
	}
	return false
}
