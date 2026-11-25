package oauth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// TokenResponse is the RFC 6749 successful token response. It deliberately has
// no id_token: this server issues no ID tokens and no JWTs.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    uint64 `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope"`
}

// handleToken serves POST /oauth/token for public clients (auth method none).
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		writeNoStore(w, http.StatusBadRequest, oidc.ErrInvalidRequest().WithDescription("cannot parse form"))
		return
	}
	// Client authentication is "none" only. A confidential-client credential
	// is refused rather than ignored, so a misconfigured caller learns why.
	if r.Form.Get("client_secret") != "" || r.Form.Get("client_assertion") != "" {
		writeNoStore(w, http.StatusUnauthorized, oidc.ErrInvalidClient().WithDescription("only public clients are supported"))
		return
	}

	var resp *TokenResponse
	var err error
	switch oidc.GrantType(r.Form.Get("grant_type")) {
	case oidc.GrantTypeCode:
		var req oidc.AccessTokenRequest
		if err = s.decoder.Decode(&req, r.Form); err != nil {
			err = oidc.ErrInvalidRequest().WithDescription("cannot parse token request")
			break
		}
		resp, err = s.exchangeCode(r.Context(), &req, r.Form["resource"])
	case oidc.GrantTypeRefreshToken:
		var req oidc.RefreshTokenRequest
		if err = s.decoder.Decode(&req, r.Form); err != nil {
			err = oidc.ErrInvalidRequest().WithDescription("cannot parse token request")
			break
		}
		resp, err = s.refresh(r.Context(), &req, r.Form["resource"])
	default:
		err = oidc.ErrUnsupportedGrantType().WithDescription("grant_type must be authorization_code or refresh_token")
	}
	if err != nil {
		s.writeTokenError(w, err)
		return
	}
	writeNoStore(w, http.StatusOK, resp)
}

// writeTokenError maps a failure to its OAuth error response. Only an
// oidc.Error reaches the client; everything else becomes server_error. Internal
// failures are logged with their cause, never with credential values.
func (s *Server) writeTokenError(w http.ResponseWriter, err error) {
	var oerr *oidc.Error
	if !errors.As(err, &oerr) {
		oerr = oidc.ErrServerError()
	}
	status := http.StatusBadRequest
	if errorCode(oerr) == string(oidc.ServerError) {
		status = http.StatusInternalServerError
		s.logger.Error("oauth token: internal failure", "error", err)
	}
	writeNoStore(w, status, oerr)
}

// exchangeCode redeems an authorization code. Checks that do not consume the
// code run first, so a wrong verifier or redirect_uri cannot burn a valid
// code. The code is then consumed atomically; a second use is a replay.
func (s *Server) exchangeCode(ctx context.Context, req *oidc.AccessTokenRequest, resources []string) (*TokenResponse, error) {
	code := req.Code
	if code == "" {
		return nil, oidc.ErrInvalidRequest().WithDescription("code is required")
	}
	clientID := req.ClientID
	if clientID == "" {
		return nil, oidc.ErrInvalidRequest().WithDescription("client_id is required")
	}
	verifier := req.CodeVerifier
	redirectURI := req.RedirectURI

	row, err := s.store.GetOAuthCode(ctx, hashToken(code))
	if errors.Is(err, storage.ErrNotFound) {
		return nil, oidc.ErrInvalidGrant().WithDescription("invalid code")
	}
	if err != nil {
		return nil, oidc.ErrServerError().WithParent(err)
	}
	if row.ConsumedAt != "" {
		// Replay: revoke every token that came from this code.
		s.revokeReplayedGrant(ctx, row.GrantID)
		return nil, oidc.ErrInvalidGrant().WithDescription("code already used")
	}
	now := s.clock()
	if expires, perr := time.Parse(timeLayout, row.ExpiresAt); perr != nil || !now.Before(expires) {
		return nil, oidc.ErrInvalidGrant().WithDescription("code expired")
	}
	if clientID != row.ClientID {
		return nil, oidc.ErrInvalidGrant().WithDescription("client_id does not match the code")
	}
	if redirectURI != row.RedirectURI {
		return nil, oidc.ErrInvalidGrant().WithDescription("redirect_uri does not match the code")
	}
	if err := op.AuthorizeCodeChallenge(verifier, &oidc.CodeChallenge{
		Challenge: row.CodeChallenge,
		Method:    oidc.CodeChallengeMethodS256,
	}); err != nil {
		return nil, asOIDCError(err)
	}

	grant, err := s.grantForCode(ctx, row.GrantID)
	if err != nil {
		return nil, err
	}
	// RFC 8707: a resource presented at token time must match the grant. The
	// authorize step already required exactly one, so it is optional here.
	if rerr := checkResourceMatchesGrant(resources, grant); rerr != nil {
		return nil, rerr
	}
	if err := s.checkUserActive(ctx, grant.UserID); err != nil {
		return nil, err
	}

	client, err := s.clients.ResolveClient(ctx, row.ClientID)
	if err != nil {
		// RFC 6749 section 5.2: the client failed to authenticate or is no
		// longer known, so the server answers invalid_client.
		return nil, oidc.ErrInvalidClient().WithDescription("client is unavailable")
	}
	return s.redeem(ctx, row, grant, now, clientMayRefresh(client))
}

// clientMayRefresh reports whether the client may receive a refresh token. A
// client with no registered grant types gets the default, code and refresh.
func clientMayRefresh(c Client) bool {
	if len(c.GrantTypes) == 0 {
		return true
	}
	return contains(c.GrantTypes, string(oidc.GrantTypeRefreshToken))
}

// refresh rotates a refresh token. A token already rotated or revoked is a
// reuse, which revokes the whole grant family.
func (s *Server) refresh(ctx context.Context, req *oidc.RefreshTokenRequest, resources []string) (*TokenResponse, error) {
	raw := req.RefreshToken
	if raw == "" {
		return nil, oidc.ErrInvalidRequest().WithDescription("refresh_token is required")
	}
	clientID := req.ClientID
	if clientID == "" {
		return nil, oidc.ErrInvalidRequest().WithDescription("client_id is required")
	}
	now := s.clock()

	row, err := s.store.GetOAuthRefreshToken(ctx, hashToken(raw))
	if errors.Is(err, storage.ErrNotFound) {
		return nil, oidc.ErrInvalidGrant().WithDescription("invalid refresh token")
	}
	if err != nil {
		return nil, oidc.ErrServerError().WithParent(err)
	}
	if clientID != row.ClientID {
		return nil, oidc.ErrInvalidGrant().WithDescription("client_id does not match the refresh token")
	}
	if row.GrantRevokedAt != "" || (row.RevokedAt != "" && row.RotatedAt == "") {
		// Revoked family (logout, deactivation, or explicit revocation).
		return nil, oidc.ErrInvalidGrant().WithDescription("refresh token is no longer valid")
	}
	if row.RotatedAt != "" {
		// A spent token presented again is reuse: revoke the whole family.
		s.revokeReplayedGrant(ctx, row.GrantID)
		return nil, oidc.ErrInvalidGrant().WithDescription("refresh token is no longer valid")
	}
	if expires, perr := time.Parse(timeLayout, row.ExpiresAt); perr != nil || !now.Before(expires) {
		return nil, oidc.ErrInvalidGrant().WithDescription("refresh token expired")
	}
	if err := s.checkUserActive(ctx, row.UserID); err != nil {
		return nil, err
	}
	client, err := s.clients.ResolveClient(ctx, row.ClientID)
	if err != nil {
		return nil, oidc.ErrInvalidClient().WithDescription("client is unavailable")
	}
	if !clientMayRefresh(client) {
		return nil, oidc.ErrUnauthorizedClient().WithDescription("client may not use refresh_token")
	}
	grant := grantFromToken(row)
	// RFC 8707: an optional resource on refresh must match the grant. The grant
	// already carries the single authorized resource, so none is required here.
	if rerr := checkResourceMatchesGrant(resources, grant); rerr != nil {
		return nil, rerr
	}
	scopes, serr := refreshScopes(req.Scopes, grant.Scopes)
	if serr != nil {
		return nil, serr
	}
	grant.Scopes = joinScopes(scopes)

	access, refreshTok, accessRow, refreshRow, err := s.mintTokens(grant, now, true)
	if err != nil {
		return nil, err
	}
	ok, err := s.store.RotateOAuthRefreshToken(ctx, row.TokenHash, accessRow, refreshRow, formatTime(now))
	if err != nil {
		return nil, oidc.ErrServerError().WithParent(err)
	}
	if !ok {
		// Lost a race with a concurrent rotation of the same token: reuse.
		s.revokeReplayedGrant(ctx, row.GrantID)
		return nil, oidc.ErrInvalidGrant().WithDescription("refresh token is no longer valid")
	}
	return tokenResponse(access, refreshTok, accessRow.ExpiresAt, scopes, now), nil
}

// redeem consumes the code and writes its tokens in one storage transaction.
// A losing concurrent redemption is a replay: its grant family is revoked
// after the winner has committed.
func (s *Server) redeem(ctx context.Context, row storage.OAuthCode, grant grantView, now time.Time, withRefresh bool) (*TokenResponse, error) {
	access, refreshTok, accessRow, refreshRow, err := s.mintTokens(grant, now, withRefresh)
	if err != nil {
		return nil, err
	}
	var refreshPtr *storage.OAuthToken
	if withRefresh {
		refreshPtr = &refreshRow
	}
	if err := s.store.RedeemOAuthCode(ctx, row.CodeHash, formatTime(now), accessRow, refreshPtr); err != nil {
		if errors.Is(err, storage.ErrOAuthCodeConsumed) {
			s.revokeReplayedGrant(ctx, row.GrantID)
			return nil, oidc.ErrInvalidGrant().WithDescription("code already used")
		}
		return nil, oidc.ErrServerError().WithParent(err)
	}
	return tokenResponse(access, refreshTok, accessRow.ExpiresAt, splitScopes(grant.Scopes), now), nil
}

// mintTokens creates opaque credentials and their storage rows. It returns the
// plaintext values (for the response only) and the hashed rows.
func (s *Server) mintTokens(grant grantView, now time.Time, withRefresh bool) (string, string, storage.OAuthToken, storage.OAuthToken, error) {
	access, err := newOpaque(prefixAccess)
	if err != nil {
		return "", "", storage.OAuthToken{}, storage.OAuthToken{}, oidc.ErrServerError().WithParent(err)
	}
	accessRow := storage.OAuthToken{
		TokenHash: hashToken(access),
		GrantID:   grant.ID,
		UserID:    grant.UserID,
		ClientID:  grant.ClientID,
		Audience:  grant.Resource,
		Scopes:    grant.Scopes,
		CreatedAt: formatTime(now),
		ExpiresAt: formatTime(now.Add(accessTokenTTL)),
	}
	var refresh string
	var refreshRow storage.OAuthToken
	if withRefresh {
		refresh, err = newOpaque(prefixRefresh)
		if err != nil {
			return "", "", storage.OAuthToken{}, storage.OAuthToken{}, oidc.ErrServerError().WithParent(err)
		}
		refreshRow = storage.OAuthToken{
			TokenHash: hashToken(refresh),
			GrantID:   grant.ID,
			UserID:    grant.UserID,
			ClientID:  grant.ClientID,
			Audience:  grant.Resource,
			Scopes:    grant.Scopes,
			CreatedAt: formatTime(now),
			ExpiresAt: formatTime(now.Add(refreshTokenTTL)),
		}
	}
	return access, refresh, accessRow, refreshRow, nil
}

// grantView is the minimal grant state the token paths need, however it was
// loaded.
type grantView struct {
	ID       string
	UserID   string
	ClientID string
	Resource string
	Scopes   string
}

func grantFromStorage(g storage.OAuthGrant) grantView {
	return grantView{ID: g.ID, UserID: g.UserID, ClientID: g.ClientID, Resource: g.Resource, Scopes: g.Scopes}
}

func grantFromToken(t storage.OAuthToken) grantView {
	return grantView{ID: t.GrantID, UserID: t.UserID, ClientID: t.ClientID, Resource: t.Audience, Scopes: t.Scopes}
}

func tokenResponse(access, refresh, accessExpires string, scopes []string, now time.Time) *TokenResponse {
	expires, _ := time.Parse(timeLayout, accessExpires)
	return &TokenResponse{
		AccessToken:  access,
		TokenType:    oidc.BearerToken,
		ExpiresIn:    uint64(expires.Sub(now).Seconds()),
		RefreshToken: refresh,
		Scope:        joinScopes(scopes),
	}
}

// grantForCode loads the grant an authorization code was issued for.
func (s *Server) grantForCode(ctx context.Context, grantID string) (grantView, error) {
	g, err := s.store.GetOAuthGrantByID(ctx, grantID)
	if err != nil {
		return grantView{}, oidc.ErrServerError().WithParent(err)
	}
	if g.RevokedAt != "" {
		return grantView{}, oidc.ErrInvalidGrant().WithDescription("grant has been revoked")
	}
	return grantFromStorage(g), nil
}

// checkResourceMatchesGrant requires a presented resource (if any) to equal
// the resource the grant was issued for. An absent resource is allowed on the
// token paths, where the grant already binds it.
func checkResourceMatchesGrant(resources []string, g grantView) *oidc.Error {
	if len(resources) == 0 {
		return nil
	}
	if len(resources) > 1 || resources[0] != g.Resource {
		return oidc.ErrInvalidTarget().WithDescription("resource does not match the grant")
	}
	return nil
}

// checkUserActive refuses a token exchange for a deactivated user. The
// grant-revocation hook already revokes tokens on deactivation; this check
// covers any gap between that write and this request.
func (s *Server) checkUserActive(ctx context.Context, userID string) *oidc.Error {
	u, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		return oidc.ErrInvalidGrant().WithDescription("user is unavailable")
	}
	if !u.Active {
		return oidc.ErrInvalidGrant().WithDescription("user is inactive")
	}
	return nil
}

// refreshScopes narrows or keeps the grant's scopes. A refresh may only ask
// for a subset of the original grant, never more.
func refreshScopes(requestedScopes oidc.SpaceDelimitedArray, granted string) ([]string, *oidc.Error) {
	if len(requestedScopes) == 0 {
		return splitScopes(granted), nil
	}
	requested := []string(requestedScopes)
	allowed := splitScopes(granted)
	for _, r := range requested {
		if !contains(allowed, r) {
			return nil, oidc.ErrInvalidScope().WithDescription("scope %q was not granted", r)
		}
	}
	return requested, nil
}

// revokeReplayedGrant revokes a whole grant family after reuse is detected,
// and records the event. A failed revocation is logged here; the client still
// gets invalid_grant, because the replay is refused either way.
func (s *Server) revokeReplayedGrant(ctx context.Context, grantID string) {
	now := formatTime(s.clock())
	err := s.store.RevokeOAuthGrant(ctx, grantID, now, storage.AuditEvent{
		OccurredAt:   now,
		Action:       "oauth.reuse_detected",
		ResourceType: "oauth_grant",
		ResourceID:   grantID,
		Outcome:      "denied",
		DetailJSON:   "{}",
	})
	if err != nil {
		s.logger.Error("oauth: revoke replayed grant failed", "error", err)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
