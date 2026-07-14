package authsrv

import (
	"encoding/json"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// ProtectedResourceMetadataPath is where RFC 9728 metadata is served; the
// bearer middleware advertises it in the 401 WWW-Authenticate challenge.
const ProtectedResourceMetadataPath = "/.well-known/oauth-protected-resource"

// protectedResourceHandler serves the RFC 9728 protected resource metadata.
func (a *AuthServer) protectedResourceHandler() http.Handler {
	return auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               a.cfg.IssuerURL,
		AuthorizationServers:   []string{a.cfg.IssuerURL},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Telegram MCP",
	})
}

// authServerMetadata is the RFC 8414 authorization server metadata document.
func (a *AuthServer) authServerMetadata() *oauthex.AuthServerMeta {
	iss := a.cfg.IssuerURL
	return &oauthex.AuthServerMeta{
		Issuer:                iss,
		AuthorizationEndpoint: iss + "/authorize",
		TokenEndpoint:         iss + "/token",
		RegistrationEndpoint:  iss + "/register",
		RevocationEndpoint:    iss + "/revoke",
		// Tokens are opaque sealed blobs, not JWS, so the key set is empty.
		// The field is still populated because AuthServerMeta serializes
		// jwks_uri unconditionally.
		JWKSURI:                           iss + "/jwks.json",
		ResponseTypesSupported:            []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported: []string{"none"},
		CodeChallengeMethodsSupported:     []string{"S256"},
	}
}

// emptyJWKS is the static document served at /jwks.json.
type emptyJWKS struct{}

func (emptyJWKS) MarshalJSON() ([]byte, error) { return []byte(`{"keys":[]}`), nil }

// jsonMetadataHandler serves a static JSON document with the same permissive
// CORS headers as auth.ProtectedResourceMetadataHandler: OAuth metadata is
// public, and browser-based MCP clients fetch it cross-origin.
func jsonMetadataHandler(v any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			http.Error(w, "encoding error", http.StatusInternalServerError)
		}
	})
}
