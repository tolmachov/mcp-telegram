package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/modelcontextprotocol/experimental-ext-variants/go/sdk/variants"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/resources"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// authCORSBypassPaths are the OAuth endpoints exempted from cross-origin
// protection: they receive legitimate cross-origin POSTs (Electron and
// browser-based MCP clients send an Origin header on token exchange). The
// MCP endpoint stays protected — it is additionally guarded by the bearer
// token. The /login/* endpoints are NOT exempted: they are called only by
// our own login page, same-origin.
var authCORSBypassPaths = []string{"/token", "/register", "/revoke"}

// buildAuthMux assembles the authenticated HTTP surface: the OAuth + login
// endpoints (unauthenticated by nature) plus the MCP handler behind
// RequireBearerToken. Extracted from runHTTPWithAuth so tests can drive the
// exact production wiring.
func buildAuthMux(as *authsrv.AuthServer, issuerURL string, mcpHandler http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	as.Routes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	requireBearer := auth.RequireBearerToken(as.Verifier(), &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: issuerURL + authsrv.ProtectedResourceMetadataPath,
	})
	mux.Handle("/", requireBearer(mcpHandler))
	return mux
}

// qrLoginFlow adapts tgclient.QRFlow to the authsrv.LoginFlow contract. The
// two packages deliberately do not know each other: tgclient stays free of
// OAuth machinery, authsrv stays free of gotd.
type qrLoginFlow struct {
	flow *tgclient.QRFlow
}

func (q qrLoginFlow) TokenURL() (string, bool)      { return q.flow.TokenURL() }
func (q qrLoginFlow) SessionData() ([]byte, bool)   { return q.flow.SessionData() }
func (q qrLoginFlow) SubmitPassword(pw string) bool { return q.flow.SubmitPassword(pw) }
func (q qrLoginFlow) Err() error                    { return q.flow.Err() }
func (q qrLoginFlow) Done() <-chan struct{}         { return q.flow.Done() }
func (q qrLoginFlow) Abort()                        { q.flow.Abort() }

func (q qrLoginFlow) State() authsrv.LoginState {
	switch q.flow.State() {
	case tgclient.QRPasswordNeeded:
		return authsrv.LoginPasswordNeeded
	case tgclient.QRDone:
		return authsrv.LoginDone
	case tgclient.QRFailed:
		return authsrv.LoginFailed
	case tgclient.QRWaiting:
		return authsrv.LoginWaiting
	default:
		// An unmapped state must fail loud, not silently poll "waiting"
		// forever until the registry TTL kills the login.
		return authsrv.LoginFailed
	}
}

func (q qrLoginFlow) User() (authsrv.LoginUser, bool) {
	u, ok := q.flow.User()
	return authsrv.LoginUser{ID: u.ID, Username: u.Username}, ok
}

// startLogin is the StartLoginFunc injected into the auth server.
func (s *Server) startLogin(ctx context.Context) (authsrv.LoginFlow, error) {
	flow, err := tgclient.StartQRLogin(ctx, s.tgConfig)
	if err != nil {
		return nil, fmt.Errorf("starting QR login: %w", err)
	}
	return qrLoginFlow{flow: flow}, nil
}

// userAssemblyBuilder returns the pool's builder: for each authenticated
// user it connects a Telegram client on their stored session and constructs
// a fresh MCP assembly on top of it.
func (s *Server) userAssemblyBuilder() userHandlerBuilder {
	return func(ctx context.Context, user *authsrv.UserIdentity) (builtAssembly, error) {
		running, err := tgclient.StartClient(ctx, s.tgConfig, s.sessionStore.Session(user.ID), s.floodWaitLogger())
		if err != nil {
			switch {
			case errors.Is(err, sessionstore.ErrCorruptSession):
				// A stored blob we cannot decrypt — almost always a key or
				// issuer misconfiguration, not a dead session. Surface it
				// loudly and preserve the blob: deleting it would make a
				// recoverable operator mistake permanent.
				s.logger.Error("stored session could not be decrypted; check AUTH_TOKEN_KEY / AUTH_ISSUER_URL", "user", user.ID, "err", err)
			case errors.Is(err, tgclient.ErrSessionUnauthorized):
				// The stored session is dead — Telegram refused a session we
				// decrypted successfully. Drop it so refresh grants stop
				// treating the user as logged in; the 401 this maps to sends
				// the client back through the QR login.
				if delErr := s.sessionStore.Delete(ctx, user.ID); delErr != nil {
					s.logger.Error("failed to delete dead session; refresh grants may loop until it is removed", "user", user.ID, "err", delErr)
				}
			}
			return builtAssembly{}, fmt.Errorf("connecting Telegram client for user %s: %w", user.ID, err)
		}

		asm, err := s.buildAssembly(running.Client())
		if err != nil {
			_ = running.Close()
			return builtAssembly{}, err
		}

		var handler http.Handler
		var closers multiCloser
		if asm.variants != nil {
			handler = variants.NewStreamableHTTPHandler(asm.variants, nil)
			vs := asm.variants
			closers = append(closers, closerFunc(func() error { return vs.Close() }))
		} else {
			srv := asm.single
			handler = mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return srv }, nil)
		}

		// Per-user pinned-chat watcher, torn down with the assembly. The 5s
		// abandon bound mirrors runHappy's shutdown path.
		watchCtx, cancelWatch := context.WithCancel(ctx)
		pinnedProvider := resources.NewPinnedChatsProvider(running.API(), asm.msgProvider, s.logger, asm.pinnedServers...)
		pinnedDone := pinnedProvider.WatchInBackground(watchCtx, s.pinnedRefresh)
		closers = append(closers, closerFunc(func() error {
			cancelWatch()
			if pinnedDone != nil {
				select {
				case <-pinnedDone:
				case <-time.After(5 * time.Second):
					s.logger.Error("pinned-chat watcher did not exit in 5s; abandoning", "user", user.ID)
				}
			}
			return nil
		}))
		closers = append(closers, closerFunc(running.Close))

		return builtAssembly{
			Handler: handler,
			Closer:  closers,
			// RunErr turns non-nil as soon as the client's Run loop exits —
			// the signal (and reason) that this assembly must stop serving.
			Health: running.RunErr,
		}, nil
	}
}

// runHTTPWithAuth serves the MCP endpoint behind the embedded OAuth
// authorization server. Every authenticated user gets their own MCP assembly
// (from the user pool) running on their own Telegram session. The auth
// endpoints themselves are mounted unauthenticated; everything else requires
// a bearer token.
func (s *Server) runHTTPWithAuth(ctx context.Context) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			s.logger.Error("auth HTTP init panic", "panic", r, "stack", string(stack))
			retErr = fmt.Errorf("auth HTTP init panic: %v", r)
		}
	}()

	as, err := authsrv.New(s.authCfg, s.logger, s.sessionStore, s.startLogin)
	if err != nil {
		return fmt.Errorf("building auth server: %w", err)
	}
	defer as.Close()

	wwwAuthenticate := fmt.Sprintf("Bearer resource_metadata=%q", s.authCfg.IssuerURL+authsrv.ProtectedResourceMetadataPath)
	pool := newUserPool(ctx, s.userAssemblyBuilder(), wwwAuthenticate, s.logger)
	defer func() {
		if closeErr := pool.Close(); closeErr != nil {
			s.logger.Warn("failed to close user pool", "err", closeErr)
		}
	}()
	go pool.janitor(ctx)

	s.logger.Info("starting with per-user Telegram authentication", "issuer", s.authCfg.IssuerURL)
	mux := buildAuthMux(as, s.authCfg.IssuerURL, pool)
	return s.serveHTTP(ctx, mux, s.httpAddr, authCORSBypassPaths...)
}
