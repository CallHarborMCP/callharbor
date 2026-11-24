package oauth

import (
	"context"
	"errors"
	"net/http"

	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/zitadel/oidc/v3/pkg/oidc"
)

// handleRevoke serves POST /oauth/revoke (RFC 7009) for public clients.
// Per RFC 7009 section 2.2 the response is 200 whether or not the token
// existed, so a caller cannot probe which tokens are valid.
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		writeNoStore(w, http.StatusBadRequest, oidc.ErrInvalidRequest().WithDescription("cannot parse form"))
		return
	}
	if r.Form.Get("client_secret") != "" || r.Form.Get("client_assertion") != "" {
		writeNoStore(w, http.StatusUnauthorized, oidc.ErrInvalidClient().WithDescription("only public clients are supported"))
		return
	}
	req := oidc.RevocationRequest{
		Token:         r.Form.Get("token"),
		TokenTypeHint: r.Form.Get("token_type_hint"),
	}
	if req.Token == "" {
		writeNoStore(w, http.StatusBadRequest, oidc.ErrInvalidRequest().WithDescription("token is required"))
		return
	}
	clientID := r.Form.Get("client_id")
	if clientID == "" {
		writeNoStore(w, http.StatusBadRequest, oidc.ErrInvalidRequest().WithDescription("client_id is required"))
		return
	}
	if err := s.revokeToken(r.Context(), req, clientID); err != nil {
		writeNoStore(w, http.StatusInternalServerError, oidc.ErrServerError())
		return
	}
	writeNoStore(w, http.StatusOK, map[string]any{})
}

// revokeToken revokes the family owning the presented token. A token belonging
// to a different client is ignored, so one client cannot revoke another's
// grant. Unknown tokens are a no-op.
func (s *Server) revokeToken(ctx context.Context, req oidc.RevocationRequest, clientID string) error {
	hash := hashToken(req.Token)
	now := formatTime(s.clock())
	owner, ok, err := s.tokenOwner(ctx, hash, req.TokenTypeHint)
	if err != nil {
		return err
	}
	if ok && owner != clientID {
		return nil
	}
	found, err := s.store.RevokeOAuthTokenByHash(ctx, hash, now)
	if err != nil {
		return err
	}
	if found {
		return s.store.InsertAudit(ctx, storage.AuditEvent{
			OccurredAt:   now,
			Action:       "oauth.token_revoked",
			ResourceType: "oauth_client",
			ResourceID:   clientID,
			Outcome:      "success",
			DetailJSON:   "{}",
		})
	}
	return nil
}

// tokenOwner returns the client a token was issued to. It is used only to
// scope revocation; it does not disclose the token's existence to the caller.
func (s *Server) tokenOwner(ctx context.Context, hash, hint string) (string, bool, error) {
	if hint != "refresh_token" {
		t, err := s.store.GetOAuthAccessToken(ctx, hash)
		if err == nil {
			return t.ClientID, true, nil
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return "", false, err
		}
	}
	if hint != "access_token" {
		t, err := s.store.GetOAuthRefreshToken(ctx, hash)
		if err == nil {
			return t.ClientID, true, nil
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return "", false, err
		}
	}
	return "", false, nil
}
