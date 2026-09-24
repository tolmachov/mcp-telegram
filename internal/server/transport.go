package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Transport names accepted by Options.Transport.
const (
	TransportStdio            = "stdio"
	TransportHTTP             = "http"
	mcpMaxBodyBytes           = 1 << 20
	httpMaxConcurrentRequests = 128
	mcpSessionTimeout         = 10 * time.Minute
)

func streamableHTTPOptions() *mcp.StreamableHTTPOptions {
	return &mcp.StreamableHTTPOptions{SessionTimeout: mcpSessionTimeout}
}

// withRequestLimits bounds both memory consumed by a single request and the
// number of long-lived HTTP/SSE requests retained by the process.
func withRequestLimits(next http.Handler) http.Handler {
	sem := make(chan struct{}, httpMaxConcurrentRequests)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		default:
			http.Error(w, "server is at request capacity", http.StatusServiceUnavailable)
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, mcpMaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// withCrossOriginProtection wraps an MCP handler in the stdlib cross-origin
// protection.
//
// go-sdk v1.6.0 disabled built-in cross-origin protection by default
// (previously on, now gated behind the enableoriginverification MCPGODEBUG
// flag until v1.8.0). Restore it explicitly via the stdlib middleware so the
// HTTP transport is not exposed to DNS-rebinding / cross-origin attacks.
// Re-evaluate this when upgrading go-sdk past v1.8.0, where the built-in
// protection returns and this middleware may become redundant.
func withCrossOriginProtection(handler http.Handler) http.Handler {
	protection := http.NewCrossOriginProtection()
	return protection.Handler(handler)
}

// serveHTTP runs an http.Server with graceful shutdown on ctx cancellation.
// The graceful drain is bounded at 5 seconds. MCP streamable clients hold
// hanging SSE GETs that never finish on their own, so hitting the bound is
// the NORMAL shutdown path with any connected client — it is logged and the
// remaining connections are force-closed, not reported as an error.
func (s *Server) serveHTTP(ctx context.Context, handler http.Handler, addr string) error {
	s.logger.Info("starting streamable HTTP server", "addr", addr)
	srv := &http.Server{
		Addr:    addr,
		Handler: withRequestLimits(handler),
		// Bound the header-read phase to blunt Slowloris-style slow-header attacks.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	shutdownDone := make(chan error, 1)
	serverExited := make(chan struct{})
	// G118: the shutdown goroutine deliberately detaches from ctx (see below);
	// it is a server-lifecycle goroutine, not a request-scoped one.
	go func() { //nolint:gosec // G118: intentional detached shutdown context
		select {
		case <-ctx.Done():
			// Shutdown deliberately starts from a fresh Background context: ctx
			// is already cancelled here, so reusing it would abort the graceful
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
