package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/modelcontextprotocol/experimental-ext-variants/go/sdk/variants"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Transport names accepted by Options.Transport.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// withCrossOriginProtection wraps handler in the stdlib cross-origin
// protection, exempting the given path patterns.
//
// go-sdk v1.6.0 disabled built-in cross-origin protection by default
// (previously on, now gated behind the enableoriginverification MCPGODEBUG
// flag until v1.8.0). Restore it explicitly via the stdlib middleware so the
// HTTP transport is not exposed to DNS-rebinding / cross-origin attacks.
// Re-evaluate this when upgrading go-sdk past v1.8.0, where the built-in
// protection returns and this middleware may become redundant.
func withCrossOriginProtection(handler http.Handler, bypass ...string) http.Handler {
	protection := http.NewCrossOriginProtection()
	for _, pattern := range bypass {
		protection.AddInsecureBypassPattern(pattern)
	}
	return protection.Handler(handler)
}

// withHealthz mounts a GET /healthz endpoint next to the MCP handler so
// container platforms (Cloud Run et al.) can probe liveness without speaking
// MCP. Everything else routes to the wrapped handler.
func withHealthz(handler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/", handler)
	return mux
}

// serveHTTP runs an http.Server with graceful shutdown on ctx cancellation.
// corsBypass patterns are exempted from cross-origin protection (used for
// OAuth endpoints that legitimately receive cross-origin form POSTs).
//
// The graceful drain is bounded at 5 seconds. MCP streamable clients hold
// hanging SSE GETs that never finish on their own, so hitting the bound is
// the NORMAL shutdown path with any connected client — it is logged and the
// remaining connections are force-closed, not reported as an error.
func (s *Server) serveHTTP(ctx context.Context, handler http.Handler, addr string, corsBypass ...string) error {
	s.logger.Info("starting streamable HTTP server", "addr", addr)
	srv := &http.Server{
		Addr:    addr,
		Handler: withCrossOriginProtection(handler, corsBypass...),
		// Bound the header-read phase to blunt Slowloris-style slow-header attacks.
		ReadHeaderTimeout: 10 * time.Second,
	}
	shutdownDone := make(chan error, 1)
	serverExited := make(chan struct{})
	// G118: the shutdown goroutine deliberately detaches from ctx (see below);
	// it is a server-lifecycle goroutine, not a request-scoped one.
	go func() { //nolint:gosec // G118: intentional detached shutdown context
		select {
		case <-ctx.Done():
			// Shutdown deliberately starts from a fresh Background context: ctx
			// is already canceled here, so reusing it would abort the graceful
			// drain immediately.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := srv.Shutdown(shutdownCtx)
			if errors.Is(err, context.DeadlineExceeded) {
				// Expected with connected streamable clients (see doc
				// comment); force-close the stragglers.
				s.logger.Info("graceful drain timed out (long-lived streams), force-closing connections")
				err = srv.Close()
			}
			shutdownDone <- err
		case <-serverExited:
			// Server exited early before ctx.Done(); send nil to unblock receiver.
			shutdownDone <- nil
		}
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		close(serverExited)
		return fmt.Errorf("http server: %w", err)
	}
	close(serverExited)
	if shutdownErr := <-shutdownDone; shutdownErr != nil {
		s.logger.Error("HTTP server shutdown failed", "err", shutdownErr)
		return fmt.Errorf("http server shutdown: %w", shutdownErr)
	}
	return nil
}

// runVariantsHTTP serves the variants.Server over streamable HTTP. Closes vs
// on return; the deferred recover wraps the whole method, so panics from
// variants.NewStreamableHTTPHandler or HTTP setup are converted to errors
// with a logged stack trace.
func (s *Server) runVariantsHTTP(ctx context.Context, vs *variants.Server, addr string) (retErr error) {
	defer func() {
		if err := vs.Close(); err != nil {
			s.logger.Warn("failed to close variants server", "err", err)
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			s.logger.Error("variants HTTP init panic", "panic", r, "stack", string(stack))
			retErr = fmt.Errorf("variants HTTP init panic: %v", r)
		}
	}()
	return s.serveHTTP(ctx, withHealthz(variants.NewStreamableHTTPHandler(vs, nil)), addr)
}

// runMCPHTTP serves a single *mcp.Server over streamable HTTP (used when the
// variants protocol is bypassed via --variant). The deferred recover mirrors
// runVariantsHTTP for symmetry — same panic risk surface.
func (s *Server) runMCPHTTP(ctx context.Context, srv *mcp.Server, addr string) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			s.logger.Error("MCP HTTP init panic", "panic", r, "stack", string(stack))
			retErr = fmt.Errorf("MCP HTTP init panic: %v", r)
		}
	}()
	handler := mcp.NewStreamableHTTPHandler(
		func(_ *http.Request) *mcp.Server { return srv },
		nil,
	)
	return s.serveHTTP(ctx, withHealthz(handler), addr)
}
