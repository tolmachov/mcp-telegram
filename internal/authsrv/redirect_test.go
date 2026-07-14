package authsrv

import "testing"

// TestRedirectPolicyAllowed is the open-redirect boundary: it pins exactly
// which redirect URIs a client may register. A regression that loosens any of
// these rejections would let an attacker register a redirect that exfiltrates
// authorization codes.
func TestRedirectPolicyAllowed(t *testing.T) {
	p := newRedirectPolicy([]string{"cursor://callback", "https://app.example.com/cb"})

	tests := []struct {
		name string
		uri  string
		want bool
	}{
		{"default claude.ai callback", "https://claude.ai/api/mcp/auth_callback", true},
		{"default claude.com callback", "https://claude.com/api/mcp/auth_callback", true},
		{"configured https extra", "https://app.example.com/cb", true},
		{"configured custom scheme", "cursor://callback", true},
		{"loopback any port", "http://127.0.0.1:52341/callback", true},
		{"localhost any port and path", "http://localhost:9/anything", true},
		{"ipv6 loopback", "http://[::1]:8080/cb", true},

		{"suffix-spoofed loopback host", "http://localhost.evil.com/cb", false},
		{"prefix-spoofed loopback host", "http://evil-localhost/cb", false},
		{"non-loopback http", "http://192.168.1.10/cb", false},
		{"public https not registered", "https://evil.example.com/cb", false},
		{"https claude subdomain spoof", "https://claude.ai.evil.com/api/mcp/auth_callback", false},
		{"loopback https not exact", "https://localhost/cb", false},
		{"loopback with fragment", "http://localhost:1234/cb#x", false},
		{"unparseable", "http://[::1", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.allowed(tc.uri); got != tc.want {
				t.Errorf("allowed(%q) = %v, want %v", tc.uri, got, tc.want)
			}
		})
	}
}

// TestMatchRegistered pins the exact-match-plus-loopback-port carve-out used
// at /authorize: a presented redirect_uri must match one registered for the
// client, exactly, except that a loopback port may differ (RFC 8252 §7.3).
func TestMatchRegistered(t *testing.T) {
	tests := []struct {
		name       string
		registered []string
		presented  string
		want       bool
	}{
		{"exact https match", []string{"https://claude.ai/cb"}, "https://claude.ai/cb", true},
		{"https differing path rejected", []string{"https://claude.ai/cb"}, "https://claude.ai/other", false},
		{"loopback differing port allowed", []string{"http://127.0.0.1:1000/cb"}, "http://127.0.0.1:2000/cb", true},
		{"loopback differing path rejected", []string{"http://127.0.0.1:1000/cb"}, "http://127.0.0.1:2000/evil", false},
		{"localhost vs 127.0.0.1 not same host", []string{"http://localhost:1000/cb"}, "http://127.0.0.1:2000/cb", false},
		{"loopback carve-out never applies to https", []string{"https://localhost/cb"}, "https://localhost/cb", true},
		{"nothing registered", nil, "http://127.0.0.1:2000/cb", false},
		{"presented not loopback, no exact", []string{"http://127.0.0.1:1000/cb"}, "http://evil.com/cb", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchRegistered(tc.registered, tc.presented); got != tc.want {
				t.Errorf("matchRegistered(%v, %q) = %v, want %v", tc.registered, tc.presented, got, tc.want)
			}
		})
	}
}
