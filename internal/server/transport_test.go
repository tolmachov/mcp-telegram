package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/keyring"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore/sessionstoretest"
	"github.com/tolmachov/mcp-telegram/internal/summarize"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// testSummarize is a valid summarisation configuration that reads no keys.
var testSummarize = summarize.Config{Provider: summarize.ProviderSampling, BatchTokens: 1}

func testServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// testAuth returns a valid embedded-OAuth configuration and an in-memory
// session store, the pair the http transport requires.
func testAuth(t *testing.T, issuer string) (*authsrv.Config, sessionstore.Store) {
	t.Helper()
	key := make([]byte, keyring.MasterKeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generating key: %v", err)
	}
	keys, err := keyring.Parse([]string{base64.StdEncoding.EncodeToString(key)})
	if err != nil {
		t.Fatalf("parsing key: %v", err)
	}
	allow, err := authsrv.ParseAllowlist([]string{"42"})
	if err != nil {
		t.Fatalf("parsing allowlist: %v", err)
	}
	return &authsrv.Config{
		IssuerURL: issuer,
		Allow:     allow,
		Keys:      keys,
	}, sessionstoretest.New(t)
}

// freePort reserves an ephemeral port and immediately releases it so
// serveHTTP (which owns its own ListenAndServe) can bind it. The tiny window
// between Close and the re-bind is an accepted test-only race.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// waitForServer polls until the HTTP server accepts connections.
func waitForServer(t *testing.T, url string) *http.Response {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:gosec,noctx // test-local URL, bounded loop
		if err == nil {
			return resp
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s did not come up within 5s", url)
	return nil
}

// TestServeHTTPServesAndShutsDown drives the production HTTP scaffolding:
// requests reach the wrapped handler, and cancelling the context shuts the
// server down cleanly.
func TestServeHTTPServesAndShutsDown(t *testing.T) {
	s := testServer(t)
	addr := freePort(t)

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.serveHTTP(ctx, handler, addr) }()

	resp := waitForServer(t, fmt.Sprintf("http://%s/anything", addr))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("wrapped handler status = %d, want 418", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveHTTP returned error on graceful shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveHTTP did not shut down within 10s of ctx cancel")
	}
}

// TestServeHTTPCrossOriginProtection verifies the DNS-rebinding defence: a
// state-changing request marked cross-site by Sec-Fetch-Site is rejected
// before reaching the handler, while same-origin requests pass.
func TestServeHTTPCrossOriginProtection(t *testing.T) {
	s := testServer(t)
	addr := freePort(t)

	reached := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.serveHTTP(ctx, withCrossOriginProtection(handler), addr) }()

	base := fmt.Sprintf("http://%s", addr)
	_ = waitForServer(t, base+"/").Body.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cross-site POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site POST status = %d, want 403", resp.StatusCode)
	}

	reached = false
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req2.Header.Set("Sec-Fetch-Site", "same-origin")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("same-origin POST: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || !reached {
		t.Errorf("same-origin POST status = %d (reached handler: %v), want 200 and reached", resp2.StatusCode, reached)
	}
}

// TestRunHTTPRejectsInvalidAuthConfig pins that authsrv's configuration
// validation fails HTTP startup before anything is served: buildAuthOptions
// leaves it to authsrv.New rather than validating twice.
func TestRunHTTPRejectsInvalidAuthConfig(t *testing.T) {
	opts := Options{
		Config:    &tgclient.Config{APIID: 1, APIHash: "hash"},
		Transport: TransportHTTP,
		HTTPAddr:  freePort(t),
		Summarize: testSummarize,
		Stdin:     strings.NewReader(""),
		Stdout:    io.Discard,
		ErrOut:    io.Discard,
	}
	opts.Auth, opts.SessionStore = testAuth(t, "http://not-loopback.example.com")
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = srv.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "invalid auth config") {
		t.Fatalf("Run = %v, want an invalid auth config error", err)
	}
}

// TestNewTransportValidation covers the Options.Transport contract in New.
func TestNewTransportValidation(t *testing.T) {
	base := func() Options {
		return Options{
			Config: &tgclient.Config{APIID: 1, APIHash: "hash"},
			Stdin:  strings.NewReader(""),
			Stdout: io.Discard,
			ErrOut: io.Discard,
		}
	}

	t.Run("http requires addr", func(t *testing.T) {
		opts := base()
		opts.Transport = TransportHTTP
		opts.Auth, opts.SessionStore = testAuth(t, "http://127.0.0.1")
		if _, err := New(opts); err == nil {
			t.Error("New accepted http transport without HTTPAddr")
		}
		opts.HTTPAddr = ":0"
		if _, err := New(opts); err != nil {
			t.Errorf("New rejected http transport with HTTPAddr: %v", err)
		}
	})

	t.Run("http requires oauth", func(t *testing.T) {
		opts := base()
		opts.Transport = TransportHTTP
		opts.HTTPAddr = ":0"
		if _, err := New(opts); err == nil {
			t.Error("New accepted http transport without Auth")
		}
	})

	t.Run("unknown transport rejected", func(t *testing.T) {
		opts := base()
		opts.Transport = "sse"
		if _, err := New(opts); err == nil {
			t.Error("New accepted unknown transport")
		}
	})
}
