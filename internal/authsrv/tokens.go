package authsrv

import "time"

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

// codeClaims is the sealed authorization code handed to the client's
// redirect URI. Subject is the decimal Telegram user ID established by the
// QR scan; /token can mint our tokens from it without any lookup. SessionID
// and SessionKey identify and decrypt this authorization's own session object
// (see the SessionKey note on accessClaims).
type codeClaims struct {
	Subject       string `json:"sub"`
	Username      string `json:"un,omitempty"`
	ClientID      string `json:"cid"`
	RedirectURI   string `json:"ru"`
	CodeChallenge string `json:"cc"`
	Resource      string `json:"res,omitempty"`
	SessionID     string `json:"sid,omitempty"`
	SessionKey    []byte `json:"sk,omitempty"`
	IssuedAt      int64  `json:"iat"`
}

// accessClaims is the payload of our bearer access token (mcp_at_...).
//
// SessionID is this authorization's session-object suffix; SessionKey is its
// per-session encryption key. SessionKey is a decryption key share, not just
// an authenticator: combined with the master key it decrypts the stored
// session, so it must never be logged and only travels inside this sealed
// token. Both are empty for pre-upgrade (legacy, master-only) sessions.
type accessClaims struct {
	Subject    string `json:"sub"`
	Username   string `json:"un,omitempty"`
	ClientID   string `json:"cid"`
	Resource   string `json:"res,omitempty"`
	SessionID  string `json:"sid,omitempty"`
	SessionKey []byte `json:"sk,omitempty"`
	IssuedAt   int64  `json:"iat"`
	ExpiresAt  int64  `json:"exp"`
}

// refreshClaims is the payload of our refresh token (mcp_rt_...). LoginAt is
// the ORIGINAL QR-login time and anchors the absolute refresh TTL: it is
// carried forward verbatim on every refresh, so re-minting never extends the
// grant's lifetime. SessionID and SessionKey are likewise carried verbatim so
// every refreshed token keeps pointing at (and decrypting) the same session
// object.
type refreshClaims struct {
	Subject    string `json:"sub"`
	Username   string `json:"un,omitempty"`
	ClientID   string `json:"cid"`
	Resource   string `json:"res,omitempty"`
	SessionID  string `json:"sid,omitempty"`
	SessionKey []byte `json:"sk,omitempty"`
	IssuedAt   int64  `json:"iat"`
	LoginAt    int64  `json:"lat"`
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
