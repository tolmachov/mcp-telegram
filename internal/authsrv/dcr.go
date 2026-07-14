package authsrv

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// maxRegistrationBody bounds the /register request body.
const maxRegistrationBody = 64 << 10

// handleRegister implements Dynamic Client Registration (RFC 7591). Nothing
// is stored: the issued client_id is an HMAC-signed blob embedding the
// registered redirect URIs, which /authorize later verifies and matches
// against. All clients are public (PKCE, no secret).
func (a *AuthServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	var meta oauthex.ClientRegistrationMetadata
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRegistrationBody)).Decode(&meta); err != nil {
		a.registrationError(w, "invalid_client_metadata", "request body is not valid JSON")
		return
	}
	if len(meta.RedirectURIs) == 0 {
		a.registrationError(w, "invalid_redirect_uri", "at least one redirect_uri is required")
		return
	}
	for _, u := range meta.RedirectURIs {
		if !a.policy.allowed(u) {
			a.registrationError(w, "invalid_redirect_uri",
				fmt.Sprintf("redirect_uri %q is not allowed: use a loopback http URI or ask the server operator to allowlist it", u))
			return
		}
	}
	for _, gt := range meta.GrantTypes {
		if gt != "authorization_code" && gt != "refresh_token" {
			a.registrationError(w, "invalid_client_metadata", fmt.Sprintf("unsupported grant_type %q", gt))
			return
		}
	}
	for _, rt := range meta.ResponseTypes {
		if rt != "code" {
			a.registrationError(w, "invalid_client_metadata", fmt.Sprintf("unsupported response_type %q", rt))
			return
		}
	}

	now := a.now()
	clientID, err := a.sealer.signClientID(clientIDClaims{
		RedirectURIs: meta.RedirectURIs,
		ClientName:   meta.ClientName,
		IssuedAt:     now.Unix(),
	})
	if err != nil {
		// Server fault, not the client's metadata: report it as such so
		// nobody burns time "fixing" valid metadata.
		a.logger.Error("signing client_id failed", "err", err)
		a.writeJSON(w, http.StatusInternalServerError, &oauthex.ClientRegistrationError{
			ErrorCode:        "server_error",
			ErrorDescription: "internal error, retry later",
		})
		return
	}

	// Echo the metadata back with the values this server enforces.
	meta.TokenEndpointAuthMethod = "none"
	meta.GrantTypes = []string{"authorization_code", "refresh_token"}
	meta.ResponseTypes = []string{"code"}
	resp := &oauthex.ClientRegistrationResponse{
		ClientRegistrationMetadata: meta,
		ClientID:                   clientID,
		ClientIDIssuedAt:           now,
	}
	a.writeJSON(w, http.StatusCreated, resp)
}

// parseClientID verifies a client_id blob and returns its embedded claims.
func (a *AuthServer) parseClientID(clientID string) (*clientIDClaims, error) {
	var claims clientIDClaims
	if err := a.sealer.verifyClientID(clientID, &claims); err != nil {
		return nil, err
	}
	return &claims, nil
}

// registrationError writes an RFC 7591 §3.2.2 error response.
func (a *AuthServer) registrationError(w http.ResponseWriter, code, description string) {
	a.writeJSON(w, http.StatusBadRequest, &oauthex.ClientRegistrationError{
		ErrorCode:        code,
		ErrorDescription: description,
	})
}

// writeJSON writes v as a JSON response with the given status. Encode
// failures cannot be reported to the client (headers are out) but are logged:
// a marshal bug must not be permanently invisible.
func (a *AuthServer) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Warn, not Debug: at the default Info level a marshal bug would
		// otherwise be permanently invisible, contradicting the intent above.
		a.logger.Warn("writing JSON response failed", "err", err)
	}
}

// clientDisplayName returns the registered client name or a fallback.
func clientDisplayName(c *clientIDClaims) string {
	if c.ClientName != "" {
		return c.ClientName
	}
	return "An MCP client"
}
