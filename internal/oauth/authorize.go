package oauth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// authPurgeBatch bounds the expired authorization rows removed per request.
const authPurgeBatch = 100

// maxStateLength matches the oauth_auth_requests CHECK on state. A longer
// value is refused before it reaches the database.
const maxStateLength = 1024

// handleAuthorize serves GET /oauth/authorize. Validation runs in the order
// that keeps the redirect safe: the client and redirect_uri are checked
// first. Only after the redirect_uri is registered may an OAuth error be sent
// to it. Anything earlier is rendered as JSON with no redirect.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	// Limit before anything else. A limited request gets a plain 429 page and
	// no redirect, so the limit cannot be used to send errors to a client.
	if ok, retry := s.authorizeLimit.allow(registrationClientIP(r, s.trustedProxies)); !ok {
		w.Header().Set("Retry-After", retryAfterSeconds(retry))
		writeNoStore(w, http.StatusTooManyRequests, registrationError{
			Error:            "temporarily_unavailable",
			ErrorDescription: "too many authorization requests; try again later",
		})
		return
	}
	ctx := r.Context()
	req, err := op.ParseAuthorizeRequest(r, s.decoder)
	if err != nil {
		s.renderError(w, err)
		return
	}
	if req.ClientID == "" {
		s.renderError(w, oidc.ErrInvalidRequest().WithDescription("client_id is required"))
		return
	}
	client, err := s.clients.ResolveClient(ctx, req.ClientID)
	if err != nil {
		s.renderError(w, oidc.ErrUnauthorizedClient().WithDescription("unknown client"))
		return
	}
	if rerr := s.checkRedirectURI(client, req.RedirectURI, req.ResponseType); rerr != nil {
		s.renderError(w, rerr)
		return
	}

	// The redirect_uri is registered from here on, so errors go back to it.
	redirect := validatedRedirect{uri: req.RedirectURI}
	fail := func(e *oidc.Error) {
		s.redirectError(w, r, redirect, req.State, e)
	}
	if req.RequestParam != "" {
		fail(oidc.ErrRequestNotSupported())
		return
	}
	if err := op.ValidateAuthReqResponseType(clientView{client}, req.ResponseType); err != nil {
		fail(asOIDCError(err))
		return
	}
	// oidc does not export unsupported_response_type, so a non-code response
	// type is reported as invalid_request with an explicit description.
	if req.ResponseType != oidc.ResponseTypeCode {
		fail(oidc.ErrInvalidRequest().WithDescription("only response_type=code is supported"))
		return
	}
	if e := checkPKCE(req); e != nil {
		fail(e)
		return
	}
	if utf8.RuneCountInString(req.State) > maxStateLength {
		fail(oidc.ErrInvalidRequest().WithDescription("state is too long"))
		return
	}
	scopes, serr := ValidateScopes(req.Scopes)
	if serr != nil {
		fail(serr)
		return
	}
	if e := ValidateResource(r.Form["resource"], s.cfg.Resource()); e != nil {
		fail(e)
		return
	}

	id, err := newOpaque(prefixRequest)
	if err != nil {
		s.logger.Error("oauth: mint request id", "error", err)
		fail(oidc.ErrServerError())
		return
	}
	now := s.clock()
	stored := storage.OAuthAuthRequest{
		ID:            id,
		ClientID:      client.ID,
		RedirectURI:   req.RedirectURI,
		State:         req.State,
		Scopes:        strings.Join(scopes, " "),
		Resource:      s.cfg.Resource(),
		CodeChallenge: req.CodeChallenge,
		CreatedAt:     formatTime(now),
		ExpiresAt:     formatTime(now.Add(authRequestTTL)),
	}
	if err := s.store.InsertOAuthAuthRequest(ctx, stored); err != nil {
		s.logger.Error("oauth: store authorization request", "error", err)
		fail(oidc.ErrServerError())
		return
	}
	// Expired requests and codes are dropped a bounded batch at a time, so
	// abandoned requests cannot accumulate. A cleanup failure must not fail
	// this request, so it is only logged.
	if err := s.store.PurgeExpiredOAuthAuth(ctx, formatTime(now), authPurgeBatch); err != nil {
		s.logger.Error("oauth: purge expired authorization state", "error", err)
	}
	http.Redirect(w, r, "/oauth/consent?request="+url.QueryEscape(id), http.StatusFound)
}

// checkRedirectURI applies the library redirect policy, then this server's
// rules: the URI must be registered exactly, and must be https or loopback
// http with no fragment or wildcard.
func (s *Server) checkRedirectURI(client Client, redirectURI string, responseType oidc.ResponseType) *oidc.Error {
	if redirectURI == "" {
		return oidc.ErrInvalidRequestRedirectURI().WithDescription("redirect_uri is required")
	}
	if err := op.ValidateAuthReqRedirectURI(clientView{client}, redirectURI, responseType); err != nil {
		return asOIDCError(err)
	}
	if !safeRedirectURI(redirectURI) {
		return oidc.ErrInvalidRequestRedirectURI().WithDescription("redirect_uri must be https, or http on loopback")
	}
	if !matchesRegisteredRedirect(client, redirectURI) {
		return oidc.ErrInvalidRequestRedirectURI().WithDescription("redirect_uri is not registered for this client")
	}
	return nil
}

// matchesRegisteredRedirect requires an exact match, except that a loopback
// URI may differ from a registered one only in its port (RFC 8252, section
// 7.3). The library's loopback check compares only the path and query, so it
// would accept a different host or scheme. This check does not.
func matchesRegisteredRedirect(client Client, redirectURI string) bool {
	req, err := url.Parse(redirectURI)
	if err != nil {
		return false
	}
	_, loopback := op.HTTPLoopbackOrLocalhost(redirectURI)
	for _, registered := range client.RedirectURIs {
		if registered == redirectURI {
			return true
		}
		if !loopback {
			continue
		}
		reg, err := url.Parse(registered)
		if err != nil {
			continue
		}
		if reg.Scheme == req.Scheme && reg.Hostname() == req.Hostname() &&
			reg.Path == req.Path && reg.RawQuery == req.RawQuery && reg.Fragment == "" {
			return true
		}
	}
	return false
}

// checkPKCE requires S256. The library helper alone accepts a missing
// challenge and the plain method, so both are refused explicitly here.
func checkPKCE(req *oidc.AuthRequest) *oidc.Error {
	if req.CodeChallenge == "" {
		return oidc.ErrInvalidRequest().WithDescription("code_challenge is required (PKCE)")
	}
	if req.CodeChallengeMethod != oidc.CodeChallengeMethodS256 {
		return oidc.ErrInvalidRequest().WithDescription("code_challenge_method must be S256")
	}
	if err := op.ValidateAuthReqCodeChallengeMethod(req.CodeChallenge, req.CodeChallengeMethod); err != nil {
		return asOIDCError(err)
	}
	if !validS256Challenge(req.CodeChallenge) {
		return oidc.ErrInvalidRequest().WithDescription("code_challenge must be 43 base64url characters")
	}
	return nil
}

// validS256Challenge reports whether s has the shape of an S256 code_challenge:
// 43 unpadded base64url characters, the encoding of a 32-byte SHA-256 digest
// (RFC 7636 section 4.2). Storage checks only the length, so the alphabet is
// checked here.
func validS256Challenge(s string) bool {
	if len(s) != 43 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// redirectError sends an OAuth error to a validated redirect_uri with state
// and iss (RFC 9207).
func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, redirect validatedRedirect, state string, e *oidc.Error) {
	target, err := s.responseURL(redirect.uri, errorParams(e, state))
	if err != nil {
		s.renderError(w, oidc.ErrServerError())
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// validatedRedirect is a redirect_uri that has passed checkRedirectURI. It
// can only be produced there, so an error redirect cannot target an
// unvalidated URI.
type validatedRedirect struct{ uri string }

// responseURL merges params and iss=<issuer> into the validated redirect_uri's
// query. The redirect_uri is parsed from storage or a validated request, so
// it is never reflected unvalidated.
func (s *Server) responseURL(redirectURI string, params url.Values) (string, error) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for k, vs := range params {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	q.Set("iss", s.cfg.Issuer())
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// renderError answers a failure that cannot be sent to a redirect_uri as a
// JSON OAuth error, with no redirect and no echo of the input.
func (s *Server) renderError(w http.ResponseWriter, err error) {
	var oerr *oidc.Error
	if !errors.As(err, &oerr) {
		oerr = oidc.ErrServerError()
	}
	writeNoStore(w, http.StatusBadRequest, oerr)
}

// clientView adapts a Client to the library's Client interface so the library
// validators run unchanged. Public clients are typed native: that is the only
// library type that permits loopback http redirects, and safeRedirectURI then
// narrows the rules further.
type clientView struct{ c Client }

func (v clientView) GetID() string                    { return v.c.ID }
func (v clientView) RedirectURIs() []string           { return v.c.RedirectURIs }
func (v clientView) PostLogoutRedirectURIs() []string { return nil }
func (v clientView) ApplicationType() op.ApplicationType {
	return op.ApplicationTypeNative
}
func (v clientView) AuthMethod() oidc.AuthMethod { return oidc.AuthMethodNone }
func (v clientView) ResponseTypes() []oidc.ResponseType {
	return []oidc.ResponseType{oidc.ResponseTypeCode}
}
func (v clientView) GrantTypes() []oidc.GrantType {
	return []oidc.GrantType{oidc.GrantTypeCode, oidc.GrantTypeRefreshToken}
}
func (v clientView) LoginURL(string) string               { return "" }
func (v clientView) AccessTokenType() op.AccessTokenType  { return op.AccessTokenTypeBearer }
func (v clientView) IDTokenLifetime() time.Duration       { return 0 }
func (v clientView) DevMode() bool                        { return false }
func (v clientView) IsScopeAllowed(string) bool           { return false }
func (v clientView) IDTokenUserinfoClaimsAssertion() bool { return false }
func (v clientView) ClockSkew() time.Duration             { return 0 }
func (v clientView) RestrictAdditionalIdTokenScopes() func([]string) []string {
	return nil
}
func (v clientView) RestrictAdditionalAccessTokenScopes() func([]string) []string {
	return nil
}

// asOIDCError returns the library error carried by err, or a server_error.
func asOIDCError(err error) *oidc.Error {
	var oerr *oidc.Error
	if errors.As(err, &oerr) {
		return oerr
	}
	return oidc.ErrServerError().WithParent(err)
}
