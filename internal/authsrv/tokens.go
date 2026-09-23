package authsrv

import (
	"time"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
)

// Artifact lifetimes. Authorization codes are single-shot by protocol but
// cannot be marked used statelessly, so their TTL is aggressive and PKCE is
// mandatory. There is no upstream token to cap against, so access tokens get
// a flat lifetime.
const (
	stateTTL       = 10 * time.Minute
	codeTTL        = 60 * time.Second
	accessTokenTTL = 55 * time.Minute
)

// stateClaims capture a validated /authorize request. They ride sealed
// through the QR-login page (bound to the pending login server-side) and
// carry everything needed to mint the authorization code once the Telegram
// login completes.
type stateClaims struct {
	ClientID      string `json:"cid"`
	RedirectURI   string `json:"ru"`
	State         string `json:"st,omitempty"`
	CodeChallenge string `json:"cc"`
	Resource      string `json:"res,omitempty"`
	IssuedAt      int64  `json:"iat"`
}

// grantClaims identify the authorization grant a code or token belongs to.
// Resource is the audience (this server). SessionID and SessionKey locate and
// decrypt the grant's own session object: SessionKey is a decryption key
// share, not just an authenticator — combined with the master key it decrypts
// the stored session — so it must never be logged and only travels inside
// sealed blobs. Family is the refresh-grant family, the authorization code's
// random id.
type grantClaims struct {
	Resource   string `json:"res,omitempty"`
	SessionID  string `json:"sid,omitempty"`
	SessionKey []byte `json:"sk,omitempty"`
	Family     string `json:"fam"`
}

// valid reports whether the grant identity is well formed for issuer: the
// session id and family are storage-safe, the session key has the minted
// length, and the resource is issuer. A failure means a forged, corrupt or
// obsolete blob; every token path checks it before the identity reaches
// storage or a new token.
func (g grantClaims) valid(issuer string) bool {
	return sessionstore.ValidSID(g.SessionID) && sessionstore.ValidSID(g.Family) &&
		len(g.SessionKey) == sessionKeyLen && normalizeResource(g.Resource) == issuer
}

// codeClaims is the sealed authorization code handed to the client's
// redirect URI. Subject is the decimal Telegram user ID established by the
// QR scan; /token can mint our tokens from it without any lookup.
type codeClaims struct {
	Subject       string `json:"sub"`
	Username      string `json:"un,omitempty"`
	ClientID      string `json:"cid"`
	RedirectURI   string `json:"ru"`
	CodeChallenge string `json:"cc"`
	grantClaims
	IssuedAt int64 `json:"iat"`
}

// accessClaims is the payload of our bearer access token (mcp_at_...).
type accessClaims struct {
	Subject  string `json:"sub"`
	Username string `json:"un,omitempty"`
	ClientID string `json:"cid"`
	grantClaims
	IssuedAt  int64 `json:"iat"`
	ExpiresAt int64 `json:"exp"`
}

// refreshClaims is the payload of our refresh token (mcp_rt_...). LoginAt is
// the ORIGINAL QR-login time and anchors the absolute refresh TTL: it is
// carried forward verbatim on every refresh, so re-minting never extends the
// grant's lifetime. The grant claims are likewise carried verbatim so every
// refreshed token keeps pointing at (and decrypting) the same session object.
type refreshClaims struct {
	Subject  string `json:"sub"`
	Username string `json:"un,omitempty"`
	ClientID string `json:"cid"`
	grantClaims
	Generation int64 `json:"gen"`
	IssuedAt   int64 `json:"iat"`
	LoginAt    int64 `json:"lat"`
}

// clientIDClaims is the HMAC-signed payload of a DCR client_id (mcp_cid_...).
// Registered redirect URIs travel inside the client_id itself, giving
// /authorize exact-match validation with no registration store.
type clientIDClaims struct {
	RedirectURIs []string `json:"ru"`
	ClientName   string   `json:"name,omitempty"`
	IssuedAt     int64    `json:"iat"`
}

// expired reports whether a moment iat+ttl has passed at time now.
func expired(iat int64, ttl time.Duration, now time.Time) bool {
	return now.After(time.Unix(iat, 0).Add(ttl))
}

// issuedAtCarrier is implemented by every claims struct so openBlob can
// enforce spec TTLs generically.
type issuedAtCarrier interface{ issuedAt() int64 }

func (c stateClaims) issuedAt() int64   { return c.IssuedAt }
func (c codeClaims) issuedAt() int64    { return c.IssuedAt }
func (c accessClaims) issuedAt() int64  { return c.IssuedAt }
func (c refreshClaims) issuedAt() int64 { return c.IssuedAt }
