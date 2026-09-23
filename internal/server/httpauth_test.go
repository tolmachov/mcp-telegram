package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// TestRunHTTPWithAuthWiring drives the production entry path (Options →
// New → Run) in the telegram-auth HTTP mode and checks the mounted surface:
// healthz, OAuth discovery, and the bearer gate on the MCP endpoint. The
// user pool is lazy, so no Telegram connection is attempted.
func TestRunHTTPWithAuthWiring(t *testing.T) {
	addr := freePort(t)
	issuer := "http://" + addr

	auth, store := testAuth(t, issuer)
	srv, err := New(Options{
		Config:       &tgclient.Config{APIID: 1, APIHash: "hash"},
		Summarize:    testSummarize,
		Transport:    TransportHTTP,
		HTTPAddr:     addr,
		Auth:         auth,
		SessionStore: store,
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

	// Cross-origin protection is scoped to the MCP endpoint. OAuth endpoints
	// remain public protocol endpoints and do not carry a custom CORS shim.
	crossSite, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("building cross-site request: %v", err)
	}
	crossSite.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err = http.DefaultClient.Do(crossSite)
	if err != nil {
		t.Fatalf("cross-site MCP request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site MCP status = %d, want 403", resp.StatusCode)
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
			Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
			Auth:      &authsrv.Config{},
			Stdin:     strings.NewReader(""),
			Stdout:    io.Discard,
			ErrOut:    io.Discard,
			Transport: TransportStdio,
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
