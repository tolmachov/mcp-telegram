package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// buildAuthMux assembles the authenticated HTTP surface: the OAuth + login
// endpoints (unauthenticated by nature) plus the MCP handler behind
// RequireBearerToken. Extracted from runHTTPWithAuth so tests can drive the
// exact production wiring.
func buildAuthMux(as *authsrv.AuthServer, issuerURL string, mcpHandler http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	as.Routes(mux)
	// Local and non-Cloud-Run deployments only: Cloud Run's frontend reserves
	// this exact path and answers it itself, so the handler is unreachable
	// there. See deploy/README.md before wiring any probe to it.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	requireBearer := auth.RequireBearerToken(as.Verifier(), &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: issuerURL + authsrv.ProtectedResourceMetadataPath,
	})
	mux.Handle("/", withCrossOriginProtection(requireBearer(mcpHandler)))
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
	case tgclient.QRPasswordNeeded, tgclient.QRPasswordVerifying:
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
	flow, err := tgclient.StartQRLogin(ctx, s.opts.Config)
	if err != nil {
		return nil, fmt.Errorf("starting QR login: %w", err)
	}
	return qrLoginFlow{flow: flow}, nil
}

// userLogger tags every log line of user's assembly, the request log
// included, with the user it serves. The pool keys an assembly by one
// authorization, so the identity it was built for is the identity of every
// request it serves.
func userLogger(base *slog.Logger, user *authsrv.UserIdentity) *slog.Logger {
	logger := base.With("user_id", user.ID.Int64())
	if user.Username != "" {
		logger = logger.With("username", user.Username)
	}
	return logger
}

// userAssemblyBuilder returns the pool's builder: for each authenticated
// user it connects a Telegram client on their stored session and constructs
// a fresh MCP assembly on top of it, which owns the client from then on. A
// session Telegram refuses comes back as ErrSessionUnauthorized, which the
// pool acts on (see userPool.dropRefusedSession).
func (s *Server) userAssemblyBuilder() userHandlerBuilder {
	return func(ctx context.Context, user *authsrv.UserIdentity) (pooledAssembly, error) {
		logger := userLogger(s.logger, user)
		running, err := tgclient.StartClient(ctx, s.opts.Config, s.opts.SessionStore.Session(user.ID, user.SessionID, user.SessionKey), logger)
		if err != nil {
			if errors.Is(err, sessionstore.ErrCorruptSession) {
				// A stored blob we cannot decrypt with this token's own session
				// key — a key/issuer misconfiguration or a tampered blob, not a
				// dead session (a token always carries its own object's key).
				// Surface it loudly; the pool preserves the blob, since deleting
				// it would make a recoverable operator mistake permanent.
				logger.Error("stored session could not be decrypted; check MCP_AUTH_TOKEN_KEYS / MCP_AUTH_ISSUER_URL", "session", user.SessionID, "err", err)
			}
			return nil, fmt.Errorf("connecting Telegram client for user %s: %w", user.ID, err)
		}
		asm, err := s.buildAssembly(ctx, running, logger)
		if err != nil {
			return nil, err
		}
		return asm, nil
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

	wwwAuthenticate := fmt.Sprintf("Bearer resource_metadata=%q", s.opts.Auth.IssuerURL+authsrv.ProtectedResourceMetadataPath)
	pool := newUserPool(ctx, s.userAssemblyBuilder(), s.opts.SessionStore.Delete, wwwAuthenticate, s.logger)
	defer func() {
		if closeErr := pool.Close(); closeErr != nil {
			s.logger.Warn("failed to close user pool", "err", closeErr)
		}
	}()
	as, err := authsrv.New(s.opts.Auth, s.logger, s.opts.SessionStore, s.startLogin, pool.EvictSession)
	if err != nil {
		return fmt.Errorf("building auth server: %w", err)
	}
	defer as.Close()
	if err := as.Start(ctx); err != nil {
		return fmt.Errorf("starting auth server: %w", err)
	}
	go pool.janitor(ctx)

	s.logger.Info("starting with per-user Telegram authentication", "issuer", s.opts.Auth.IssuerURL)
	mux := buildAuthMux(as, s.opts.Auth.IssuerURL, pool)
	return s.serveHTTP(ctx, mux, s.opts.HTTPAddr, httpDrainTimeout)
}
