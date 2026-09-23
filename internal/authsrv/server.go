package authsrv

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// AuthServer is the embedded OAuth 2.1 authorization server protecting the
// MCP HTTP transport.
type AuthServer struct {
	cfg         *Config
	sealer      *sealer
	policy      *redirectPolicy
	limiter     *ipRateLimiter
	logger      *slog.Logger
	store       sessionstore.Store
	startLogin  StartLoginFunc
	now         func() time.Time
	lifecycleMu sync.Mutex
	started     bool
	closed      bool

	// loginCtx carries values to login flows and signals shutdown to the
	// janitor and the orphan-session sweeper; canceling it (via Close) stops
	// both. It does NOT abort in-flight QR flows — those detach from any
	// context (see StartLoginFunc) and are stopped explicitly by Close
	// draining the pending registry and calling Abort on each flow.
	loginCtx    context.Context
	cancelLogin context.CancelFunc
	janitorDone chan struct{}
	sweepDone   chan struct{}

	// invalidate tears down any live client assembly for a (userID, sid)
	// session so it cannot re-store the blob after we delete it.
	invalidate func(userID tgid.UserID, sid string)

	pendingMu sync.Mutex
	pending   map[string]*pendingLogin
}

// New validates cfg and builds the authorization server. store persists the
// per-authorization Telegram sessions produced by successful QR logins (and
// gates the refresh grant); startLogin launches one QR login flow per /authorize.
// Callers must Close the returned server to stop the pending-login janitor
// and abort in-flight logins.
func New(
	cfg *Config,
	logger *slog.Logger,
	store sessionstore.Store,
	startLogin StartLoginFunc,
	invalidate func(userID tgid.UserID, sid string),
) (*AuthServer, error) {
	// Work on a private copy: a caller mutating cfg after construction must
	// not desynchronize the sealer's AAD from the metadata endpoints.
	if cfg == nil {
		return nil, fmt.Errorf("invalid auth config: auth config must not be nil")
	}
	cfgCopy := cfg.Normalized()
	if err := cfgCopy.Validate(); err != nil {
		return nil, fmt.Errorf("invalid auth config: %w", err)
	}
	if store == nil {
		return nil, fmt.Errorf("session store is required")
	}
	if startLogin == nil {
		return nil, fmt.Errorf("start-login function is required")
	}
	if invalidate == nil {
		return nil, fmt.Errorf("session invalidator is required")
	}
	ring, err := newKeyRing(cfgCopy.TokenKeys)
	if err != nil {
		return nil, fmt.Errorf("building key ring: %w", err)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	a := &AuthServer{
		cfg:        &cfgCopy,
		sealer:     newSealer(ring, cfgCopy.IssuerURL),
		policy:     newRedirectPolicy(cfgCopy.ExtraRedirects),
		limiter:    newIPRateLimiter(rateLimitPerSecond, rateLimitBurst, cfgCopy.TrustedProxyHops),
		logger:     logger,
		store:      store,
		startLogin: startLogin,
		now:        time.Now,
		invalidate: invalidate,
		pending:    map[string]*pendingLogin{},
	}
	return a, nil
}

// Start launches the independent login and storage maintenance loops. It is
// separated from New so all collaborators are wired before background work can
// observe the server. Repeated calls are harmless.
func (a *AuthServer) Start(parent context.Context) error {
	if parent == nil {
		return fmt.Errorf("auth server start context is required")
	}
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	if a.closed {
		return fmt.Errorf("auth server is closed")
	}
	if a.started {
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	a.loginCtx = ctx
	a.cancelLogin = cancel
	a.janitorDone = make(chan struct{})
	a.sweepDone = make(chan struct{})
	a.started = true
	go a.janitor(ctx)
	go a.sessionSweeper(ctx)
	return nil
}

// Close stops the janitor and the orphan-session sweeper, then aborts every
// in-flight Telegram login. Idempotent.
func (a *AuthServer) Close() {
	a.lifecycleMu.Lock()
	if a.closed {
		a.lifecycleMu.Unlock()
		return
	}
	a.closed = true
	started := a.started
	cancel := a.cancelLogin
	janitorDone := a.janitorDone
	sweepDone := a.sweepDone
	a.lifecycleMu.Unlock()
	if started {
		cancel()
		<-janitorDone
		<-sweepDone
	}

	a.pendingMu.Lock()
	stale := make([]*pendingLogin, 0, len(a.pending))
	for id, p := range a.pending {
		delete(a.pending, id)
		stale = append(stale, p)
	}
	a.pendingMu.Unlock()
	for _, p := range stale {
		if p.flow != nil {
			p.flow.Abort()
		}
	}
}

// Routes mounts every auth endpoint on mux. The MCP handler itself is mounted
// by the caller (wrapped in RequireBearerToken with this server's Verifier).
func (a *AuthServer) Routes(mux *http.ServeMux) {
	rl := a.limiter
	mux.Handle("GET /.well-known/oauth-protected-resource", a.protectedResourceHandler())
	mux.Handle("GET /.well-known/oauth-authorization-server", a.jsonMetadataHandler(a.authServerMetadata()))
	// Some clients probe the OIDC discovery path as a fallback; serve the
	// same document there.
	mux.Handle("GET /.well-known/openid-configuration", a.jsonMetadataHandler(a.authServerMetadata()))
	mux.Handle("GET /jwks.json", a.jsonMetadataHandler(emptyJWKS{}))
	mux.Handle("POST /register", rl.wrap(http.HandlerFunc(a.handleRegister)))
	mux.Handle("GET /authorize", rl.wrap(http.HandlerFunc(a.handleAuthorize)))
	mux.Handle("GET /login/qr", rl.wrap(http.HandlerFunc(a.handleLoginQR)))
	mux.Handle("GET /login/poll", rl.wrap(http.HandlerFunc(a.handleLoginPoll)))
	mux.Handle("POST /login/password", rl.wrap(http.HandlerFunc(a.handleLoginPassword)))
	mux.Handle("POST /token", rl.wrap(http.HandlerFunc(a.handleToken)))
	mux.Handle("POST /revoke", rl.wrap(http.HandlerFunc(a.handleRevoke)))
}
