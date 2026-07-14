package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
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

// TestServeHTTPHealthzAndShutdown drives the production HTTP scaffolding:
// healthz responds, unknown paths reach the wrapped handler, and canceling
// the context shuts the server down cleanly.
func TestServeHTTPHealthzAndShutdown(t *testing.T) {
	s := testServer(t)
	addr := freePort(t)

	handler := withHealthz(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.serveHTTP(ctx, handler, addr) }()

	base := fmt.Sprintf("http://%s", addr)
	resp := waitForServer(t, base+"/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok" {
		t.Errorf("healthz body = %q, want %q", body, "ok")
	}

	resp2, err := http.Get(base + "/anything") //nolint:gosec,noctx // test-local URL
	if err != nil {
		t.Fatalf("GET /anything: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusTeapot {
		t.Errorf("wrapped handler status = %d, want 418", resp2.StatusCode)
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

// TestServeHTTPCrossOriginProtection verifies the DNS-rebinding defense: a
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
	go func() { done <- s.serveHTTP(ctx, handler, addr) }()

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

	t.Run("empty defaults to stdio", func(t *testing.T) {
		opts := base()
		srv, err := New(opts)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if srv.transport != TransportStdio {
			t.Errorf("transport = %q, want %q", srv.transport, TransportStdio)
		}
	})

	t.Run("http requires addr", func(t *testing.T) {
		opts := base()
		opts.Transport = TransportHTTP
		if _, err := New(opts); err == nil {
			t.Error("New accepted http transport without HTTPAddr")
		}
		opts.HTTPAddr = ":0"
		if _, err := New(opts); err != nil {
			t.Errorf("New rejected http transport with HTTPAddr: %v", err)
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
