package authsrv

import (
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// ProtectedResourceMetadataPath is where RFC 9728 metadata is served; the
// bearer middleware advertises it in the 401 WWW-Authenticate challenge.
const ProtectedResourceMetadataPath = "/.well-known/oauth-protected-resource"

// protectedResourceHandler serves the RFC 9728 protected resource metadata.
func (a *AuthServer) protectedResourceHandler() http.Handler {
	return a.jsonMetadataHandler(&oauthex.ProtectedResourceMetadata{
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

// jsonMetadataHandler serves a static JSON document.
func (a *AuthServer) jsonMetadataHandler(v any) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		a.writeJSON(w, http.StatusOK, v)
	})
}
