package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// TestRunHTTPWithAuthWiring drives the production entry path (Options →
// New → Run) in the telegram-auth HTTP mode and checks the mounted surface:
// healthz, OAuth discovery, and the bearer gate on the MCP endpoint. The
// user pool is lazy, so no Telegram connection is attempted.
func TestRunHTTPWithAuthWiring(t *testing.T) {
	addr := freePort(t)
	issuer := "http://" + addr

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generating key: %v", err)
	}
	srv, err := New(Options{
		Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
		Transport: TransportHTTP,
		HTTPAddr:  addr,
		Auth: &authsrv.Config{
			IssuerURL: issuer,
			Allow:     authsrv.AllowUsers(42),
			TokenKeys: []string{base64.StdEncoding.EncodeToString(key)},
		},
		SessionStore: sessionstore.NewMemory(),
		Stdin:        strings.NewReader(""),
		Stdout:       io.Discard,
		ErrOut:       io.Discard,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	base := fmt.Sprintf("http://%s", addr)
	resp := waitForServer(t, base+"/healthz")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", resp.StatusCode)
	}

	// OAuth discovery must be reachable without authentication.
	resp, err = http.Get(base + authsrv.ProtectedResourceMetadataPath) //nolint:noctx // test-local URL
	if err != nil {
		t.Fatalf("GET protected resource metadata: %v", err)
	}
	var metadata struct {
		Resource string `json:"resource"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		t.Fatalf("decoding metadata: %v", err)
	}
	_ = resp.Body.Close()
	if metadata.Resource != issuer {
		t.Errorf("metadata resource = %q, want %q", metadata.Resource, issuer)
	}

	// The MCP endpoint must reject unauthenticated requests and advertise
	// the resource metadata for OAuth discovery.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST MCP without bearer: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("MCP without bearer: status = %d, want 401", resp.StatusCode)
	}
	if h := resp.Header.Get("WWW-Authenticate"); !strings.Contains(h, authsrv.ProtectedResourceMetadataPath) {
		t.Errorf("WWW-Authenticate = %q, want it to point at resource metadata", h)
	}

	// CORS-bypass wiring: OAuth endpoints must accept cross-origin POSTs (or
	// browser/Electron token exchange breaks with 403), while /login/password
	// must NOT be bypassed (or 2FA submission becomes CSRF-able). We assert on
	// the status code being anything other than 403 for the bypassed path and
	// exactly 403 for the protected one.
	crossSitePOST := func(path string) int {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatalf("cross-site POST %s: %v", path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := crossSitePOST("/token"); code == http.StatusForbidden {
		t.Error("/token is cross-origin blocked; token exchange from browser clients would break")
	}
	if code := crossSitePOST("/login/password"); code != http.StatusForbidden {
		t.Errorf("/login/password cross-site status = %d, want 403 (must not be CORS-bypassed)", code)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error on shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not shut down within 10s of ctx cancel")
	}
}

// TestNewAuthValidation covers the Options.Auth contract in New.
func TestNewAuthValidation(t *testing.T) {
	base := func() Options {
		return Options{
			Config: &tgclient.Config{APIID: 1, APIHash: "hash"},
			Auth:   &authsrv.Config{},
			Stdin:  strings.NewReader(""),
			Stdout: io.Discard,
			ErrOut: io.Discard,
		}
	}

	t.Run("auth requires http transport", func(t *testing.T) {
		opts := base()
		if _, err := New(opts); err == nil {
			t.Error("New accepted Auth with the stdio transport")
		}
	})

	t.Run("auth requires session store", func(t *testing.T) {
		opts := base()
		opts.Transport = TransportHTTP
		opts.HTTPAddr = ":0"
		if _, err := New(opts); err == nil {
			t.Error("New accepted Auth without a SessionStore")
		}
	})
}
