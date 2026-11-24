package oauth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// touchInterval bounds how often a token use is written. The grant's
// last_used_at only needs minute precision for the grants list.
const touchInterval = time.Minute

// requiredScopesHeader is the scope set advertised on 401 challenges.
const requiredScopesHeader = "mcp:read mcp:call"

// Principal is the authenticated caller of a protected resource.
type Principal struct {
	UserID   string
	ClientID string
	Scopes   []string
	GrantID  string
}

type principalKey struct{}

// PrincipalFromContext returns the principal RequireBearer stored, if any.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// protectedResourceMetadataURL is the RFC 9728 document for this resource.
func (s *Server) protectedResourceMetadataURL() string {
	return s.cfg.Issuer() + "/.well-known/oauth-protected-resource/mcp"
}

// challenge writes a Bearer challenge. An empty errCode is the "no
// credentials" form, which carries no error attribute.
func (s *Server) challenge(w http.ResponseWriter, status int, errCode, scope string) {
	parts := []string{}
	if errCode != "" {
		parts = append(parts, `error="`+errCode+`"`)
	}
	if scope != "" {
		parts = append(parts, `scope="`+scope+`"`)
	}
	parts = append(parts, `resource_metadata="`+s.protectedResourceMetadataURL()+`"`)
	w.Header().Set("WWW-Authenticate", "Bearer "+strings.Join(parts, ", "))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
}

// bearerToken extracts the credential from an Authorization header only.
// Query and form parameters are never accepted.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	raw := r.Header.Get("Authorization")
	if len(raw) <= len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(raw[len(prefix):])
	return tok, tok != ""
}

// RequireBearer protects a handler with an OAuth access token. Every failure
// before the scope check is a 401 invalid_token (or no error code when no
// token was sent). A token that lacks a required scope gets 403.
func (s *Server) RequireBearer(scopes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok, ok := bearerToken(r)
			if !ok {
				s.challenge(w, http.StatusUnauthorized, "", "")
				return
			}
			p, status, errCode := s.authenticateAccessToken(r.Context(), tok)
			if status != 0 {
				s.challenge(w, status, errCode, requiredScopesHeader)
				return
			}
			if missing := missingScope(p.Scopes, scopes); missing != "" {
				s.challenge(w, http.StatusForbidden, "insufficient_scope", missing)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
		})
	}
}

// missingScope returns the first required scope not in granted, or "".
func missingScope(granted, required []string) string {
	for _, want := range required {
		if !contains(granted, want) {
			return want
		}
	}
	return ""
}

// authenticateAccessToken checks one presented token. It returns a non-zero
// HTTP status and error code on failure. Every token rejection uses the same
// invalid_token answer, so the reason is not disclosed to the caller.
func (s *Server) authenticateAccessToken(ctx context.Context, raw string) (Principal, int, string) {
	const invalid = http.StatusUnauthorized
	row, err := s.store.GetOAuthAccessToken(ctx, hashToken(raw))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Principal{}, invalid, "invalid_token"
		}
		return Principal{}, http.StatusInternalServerError, "server_error"
	}
	now := s.clock()
	expires, perr := time.Parse(timeLayout, row.ExpiresAt)
	if row.RevokedAt != "" || row.GrantRevokedAt != "" || perr != nil || !now.Before(expires) {
		return Principal{}, invalid, "invalid_token"
	}
	if row.Audience != s.cfg.Resource() {
		return Principal{}, invalid, "invalid_token"
	}
	u, err := s.store.GetUserByID(ctx, row.UserID)
	if err != nil || !u.Active {
		return Principal{}, invalid, "invalid_token"
	}
	s.touchGrant(ctx, row.GrantID, now)
	return Principal{
		UserID:   row.UserID,
		ClientID: row.ClientID,
		Scopes:   splitScopes(row.Scopes),
		GrantID:  row.GrantID,
	}, 0, ""
}

// touchGrant records a use of the grant, at most once per touchInterval. The
// write is best effort: a failure must not reject an otherwise valid request.
func (s *Server) touchGrant(ctx context.Context, grantID string, now time.Time) {
	before := formatTime(now.Add(-touchInterval))
	_ = s.store.TouchOAuthGrant(ctx, grantID, formatTime(now), before)
}
