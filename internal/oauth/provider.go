// Package oauth implements the CallHarbor OAuth 2.1 authorization server used
// by remote MCP clients, and the protected-resource checks for /mcp.
//
// Protocol parsing and validation use github.com/zitadel/oidc/v3 types and
// helpers (oidc.AuthRequest, oidc.AccessTokenRequest, oidc.Error,
// op.ValidateAuthReqRedirectURI and friends). This package owns only storage,
// opaque token minting (crypto/rand, hashed at rest), rotation and grant
// families, and the MCP-specific resource and issuer rules.
//
// No JWTs or ID tokens are issued, so the server needs no signing key.
package oauth

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

const (
	// ScopeRead and ScopeCall are the only scopes this server grants.
	ScopeRead = "mcp:read"
	ScopeCall = "mcp:call"

	authRequestTTL   = 10 * time.Minute
	codeTTL          = 60 * time.Second
	accessTokenTTL   = time.Hour
	refreshTokenTTL  = 30 * 24 * time.Hour
	maxScopeLength   = 512
	maxResourceBytes = 2048

	// Opaque credential prefixes make leaked values recognizable in logs and
	// secret scanners. They carry no meaning for the server.
	prefixCode    = "cho_c_"
	prefixAccess  = "cho_a_"
	prefixRefresh = "cho_r_"
	prefixGrant   = "cho_g_"
	prefixRequest = "cho_q_"
)

// supportedScopes is the complete scope vocabulary. Anything else is
// invalid_scope, never silently dropped.
var supportedScopes = []string{ScopeRead, ScopeCall}

// Clock lets tests drive expiry deterministically.
type Clock func() time.Time

// Client is the protocol view of a registered client, resolved per request.
type Client struct {
	ID           string
	Name         string
	URI          string
	RedirectURIs []string
	// GrantTypes are the grant types the client registered. An empty list means
	// the default (authorization_code and refresh_token).
	GrantTypes []string
	// MetadataDocument is true when the client was resolved from a Client ID
	// Metadata Document fetched from its client_id URL.
	MetadataDocument bool
}

// ClientResolver returns the client for a client_id. Implementations must
// return ErrUnknownClient for an identifier they do not know.
type ClientResolver interface {
	ResolveClient(ctx context.Context, clientID string) (Client, error)
}

// ErrUnknownClient reports a client_id that is not registered.
var ErrUnknownClient = errors.New("oauth: unknown client")

// Store is the persistence surface the protocol handlers use. The concrete
// *storage.Store satisfies it; tests may wrap it.
type Store interface {
	GetOAuthClient(ctx context.Context, clientID string) (storage.OAuthClient, error)
	UpsertOAuthClient(ctx context.Context, c storage.OAuthClient) error
	InsertDynamicOAuthClient(ctx context.Context, c storage.OAuthClient, audit storage.AuditEvent) error
	TouchOAuthGrant(ctx context.Context, grantID, now, before string) error
	CountDynamicOAuthClients(ctx context.Context, sinceISO string) (int, error)
	CountMetadataOAuthClients(ctx context.Context) (int, error)
	InsertOAuthAuthRequest(ctx context.Context, r storage.OAuthAuthRequest) error
	PurgeExpiredOAuthAuth(ctx context.Context, now string, limit int) error
	GetOAuthAuthRequest(ctx context.Context, id string) (storage.OAuthAuthRequest, error)
	DecideOAuthAuthRequest(ctx context.Context, id, userID, now string) error
	ApproveOAuthRequest(ctx context.Context, requestID, userID string, grant storage.OAuthGrant, code storage.OAuthCode, now string, audit storage.AuditEvent) error
	DecideAndApproveOAuthRequest(ctx context.Context, requestID, userID string, grant storage.OAuthGrant, code storage.OAuthCode, now string, audit storage.AuditEvent) error
	DenyOAuthRequest(ctx context.Context, requestID, userID, now string, audit storage.AuditEvent) error
	GetOAuthCode(ctx context.Context, codeHash string) (storage.OAuthCode, error)
	RedeemOAuthCode(ctx context.Context, codeHash, now string, access storage.OAuthToken, refresh *storage.OAuthToken) error
	GetOAuthAccessToken(ctx context.Context, tokenHash string) (storage.OAuthToken, error)
	GetOAuthGrantByID(ctx context.Context, grantID string) (storage.OAuthGrant, error)
	ListOAuthGrants(ctx context.Context, userID string) ([]storage.OAuthGrant, error)
	GetOAuthGrantForUser(ctx context.Context, userID, grantID string) (storage.OAuthGrant, error)
	GetOAuthRefreshToken(ctx context.Context, tokenHash string) (storage.OAuthToken, error)
	RotateOAuthRefreshToken(ctx context.Context, oldHash string, next storage.OAuthToken, nextRefresh storage.OAuthToken, now string) (bool, error)
	RevokeOAuthGrant(ctx context.Context, grantID, now string, audit storage.AuditEvent) error
	RevokeOAuthTokenByHash(ctx context.Context, tokenHash, now string) (bool, error)
	GetUserByID(ctx context.Context, id string) (storage.User, error)
	InsertAudit(ctx context.Context, e storage.AuditEvent) error
}

// Config is the fixed server identity. Issuer and Resource are derived only
// from the configured public origin, never from request headers.
type Config struct {
	// PublicURL is the normalized origin, e.g. https://callharbor.example.com.
	PublicURL string
}

// Issuer is the exact issuer identifier and the RFC 9207 iss value.
func (c Config) Issuer() string { return c.PublicURL }

// Resource is the single protected resource this server accepts tokens for.
func (c Config) Resource() string { return c.PublicURL + "/mcp" }

// ValidateScopes returns the requested scopes if all are supported, or an
// invalid_scope error naming the first unsupported one. An empty request
// means the client did not ask for scopes, so it receives all of them.
func ValidateScopes(requested []string) ([]string, *oidc.Error) {
	if len(requested) == 0 {
		return append([]string(nil), supportedScopes...), nil
	}
	seen := make(map[string]bool, len(requested))
	out := make([]string, 0, len(requested))
	for _, s := range requested {
		if !isSupportedScope(s) {
			return nil, oidc.ErrInvalidScope().WithDescription("unsupported scope %q", s)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

func isSupportedScope(s string) bool {
	for _, supported := range supportedScopes {
		if s == supported {
			return true
		}
	}
	return false
}

// ValidateResource enforces RFC 8707 for this server: exactly one resource
// parameter, equal to the configured MCP resource.
func ValidateResource(resources []string, want string) *oidc.Error {
	switch {
	case len(resources) == 0:
		return oidc.ErrInvalidTarget().WithDescription("resource parameter is required")
	case len(resources) > 1:
		return oidc.ErrInvalidTarget().WithDescription("exactly one resource parameter is allowed")
	case resources[0] != want:
		return oidc.ErrInvalidTarget().WithDescription("unsupported resource")
	}
	return nil
}

// safeRedirectURI reports whether a registered redirect URI is acceptable for
// a public client under this server's policy: https, or http only on loopback,
// with no fragment and no wildcard.
func safeRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Host == "" || u.User != nil || strings.ContainsAny(raw, "*") {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		_, loopback := op.HTTPLoopbackOrLocalhost(raw)
		return loopback
	}
	return false
}
