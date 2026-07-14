package authsrv

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
)

// AuthServer is the embedded OAuth 2.1 authorization server protecting the
// MCP HTTP transport.
type AuthServer struct {
	cfg        *Config
	sealer     *sealer
	policy     *redirectPolicy
	limiter    *ipRateLimiter
	logger     *slog.Logger
	store      sessionstore.Store
	startLogin StartLoginFunc
	now        func() time.Time

	// loginCtx carries values to login flows and signals shutdown to the
	// janitor; canceling it (via Close) stops the janitor. It does NOT abort
	// in-flight QR flows — those detach from any context (see StartLoginFunc)
	// and are stopped explicitly by Close draining the pending registry and
	// calling Abort on each flow.
	loginCtx    context.Context
	cancelLogin context.CancelFunc
	janitorDone chan struct{}

	pendingMu sync.Mutex
	pending   map[string]*pendingLogin
}

// New validates cfg and builds the authorization server. store persists the
// per-user Telegram sessions produced by successful QR logins (and gates the
// refresh grant); startLogin launches one QR login flow per /authorize.
// Callers must Close the returned server to stop the pending-login janitor
// and abort in-flight logins.
func New(cfg *Config, logger *slog.Logger, store sessionstore.Store, startLogin StartLoginFunc) (*AuthServer, error) {
	// Work on a private copy: a caller mutating cfg after construction must
	// not desynchronize the sealer's AAD from the metadata endpoints.
	cfgCopy := *cfg
	if err := cfgCopy.Validate(); err != nil {
		return nil, fmt.Errorf("invalid auth config: %w", err)
	}
	if store == nil {
		return nil, fmt.Errorf("session store is required")
	}
	if startLogin == nil {
		return nil, fmt.Errorf("start-login function is required")
	}
	ring, err := newKeyRing(cfgCopy.TokenKeys)
	if err != nil {
		return nil, fmt.Errorf("building key ring: %w", err)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &AuthServer{
		cfg:         &cfgCopy,
		sealer:      newSealer(ring, cfgCopy.IssuerURL),
		policy:      newRedirectPolicy(cfgCopy.ExtraRedirects),
		limiter:     newIPRateLimiter(rateLimitPerSecond, rateLimitBurst),
		logger:      logger,
		store:       store,
		startLogin:  startLogin,
		now:         time.Now,
		loginCtx:    ctx,
		cancelLogin: cancel,
		janitorDone: make(chan struct{}),
		pending:     map[string]*pendingLogin{},
	}
	go a.janitor(ctx)
	return a, nil
}

// Close stops the janitor and aborts every in-flight Telegram login. Idempotent.
func (a *AuthServer) Close() {
	a.cancelLogin()
	<-a.janitorDone

	a.pendingMu.Lock()
	stale := make([]*pendingLogin, 0, len(a.pending))
	for id, p := range a.pending {
		delete(a.pending, id)
		stale = append(stale, p)
	}
	a.pendingMu.Unlock()
	for _, p := range stale {
		p.flow.Abort()
	}
}

// Routes mounts every auth endpoint on mux. The MCP handler itself is mounted
// by the caller (wrapped in RequireBearerToken with this server's Verifier).
func (a *AuthServer) Routes(mux *http.ServeMux) {
	rl := a.limiter
	mux.Handle("GET /.well-known/oauth-protected-resource", a.protectedResourceHandler())
	mux.Handle("GET /.well-known/oauth-authorization-server", jsonMetadataHandler(a.authServerMetadata()))
	// Some clients probe the OIDC discovery path as a fallback; serve the
	// same document there.
	mux.Handle("GET /.well-known/openid-configuration", jsonMetadataHandler(a.authServerMetadata()))
	mux.Handle("GET /jwks.json", jsonMetadataHandler(emptyJWKS{}))
	mux.Handle("POST /register", rl.wrap(http.HandlerFunc(a.handleRegister)))
	mux.Handle("GET /authorize", rl.wrap(http.HandlerFunc(a.handleAuthorize)))
	mux.Handle("GET /login/qr", rl.wrap(http.HandlerFunc(a.handleLoginQR)))
	mux.Handle("GET /login/poll", rl.wrap(http.HandlerFunc(a.handleLoginPoll)))
	mux.Handle("POST /login/password", rl.wrap(http.HandlerFunc(a.handleLoginPassword)))
	mux.Handle("POST /token", rl.wrap(http.HandlerFunc(a.handleToken)))
	mux.Handle("POST /revoke", rl.wrap(http.HandlerFunc(a.handleRevoke)))
}
