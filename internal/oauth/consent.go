package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/zitadel/oidc/v3/pkg/oidc"
)

// ErrRequestNotFound reports an authorization request that is unknown,
// expired, already decided or already consumed. Callers must not distinguish
// these cases to the browser.
var ErrRequestNotFound = errors.New("oauth: authorization request not found")

// ConsentRequest is the browser-facing view of a pending authorization request.
type ConsentRequest struct {
	ID         string
	ClientID   string
	ClientName string
	ClientURI  string
	Scopes     []string
	Resource   string
	ExpiresAt  time.Time
}

// LoadConsentRequest returns the pending request for consent. Unknown,
// expired and already decided requests all return ErrRequestNotFound.
func (s *Server) LoadConsentRequest(ctx context.Context, id string) (ConsentRequest, error) {
	r, err := s.store.GetOAuthAuthRequest(ctx, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ConsentRequest{}, ErrRequestNotFound
		}
		return ConsentRequest{}, err
	}
	expires, err := time.Parse(timeLayout, r.ExpiresAt)
	if err != nil {
		return ConsentRequest{}, fmt.Errorf("oauth: parse expiry: %w", err)
	}
	if !s.clock().Before(expires) || r.DecidedAt != "" || r.ConsumedAt != "" {
		return ConsentRequest{}, ErrRequestNotFound
	}
	return ConsentRequest{
		ID:         r.ID,
		ClientID:   r.ClientID,
		ClientName: r.ClientName,
		ClientURI:  "",
		Scopes:     splitScopes(r.Scopes),
		Resource:   r.Resource,
		ExpiresAt:  expires,
	}, nil
}

// Decide records the signed-in user's approve or deny decision and returns the
// URL to redirect the browser to. The decision is bound to userID, is
// single-use, and only succeeds for an unexpired pending request.
func (s *Server) Decide(ctx context.Context, requestID, userID string, approve bool) (string, error) {
	now := s.clock()
	nowText := formatTime(now)

	req, err := s.store.GetOAuthAuthRequest(ctx, requestID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", ErrRequestNotFound
		}
		return "", err
	}
	expires, err := time.Parse(timeLayout, req.ExpiresAt)
	if err != nil {
		return "", fmt.Errorf("oauth: parse expiry: %w", err)
	}
	if !now.Before(expires) || req.DecidedAt != "" || req.ConsumedAt != "" {
		return "", ErrRequestNotFound
	}
	// A deactivated user cannot grant consent, even with a live session.
	u, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		return "", ErrRequestNotFound
	}
	if !u.Active {
		return "", ErrRequestNotFound
	}

	audit := storage.AuditEvent{
		OccurredAt:   nowText,
		ActorUserID:  userID,
		TargetUserID: userID,
		Action:       "oauth.consent",
		ResourceType: "oauth_client",
		ResourceID:   req.ClientID,
		DetailJSON:   "{}",
	}

	if !approve {
		audit.Outcome = "denied"
		if err := s.store.DenyOAuthRequest(ctx, requestID, userID, nowText, audit); err != nil {
			return "", mapDecisionErr(err)
		}
		return s.responseURL(req.RedirectURI, errorParams(oidc.ErrAccessDenied(), req.State))
	}

	code, err := newOpaque(prefixCode)
	if err != nil {
		return "", err
	}
	grantID, err := newOpaque(prefixGrant)
	if err != nil {
		return "", err
	}
	audit.Outcome = "success"
	audit.ResourceID = grantID

	grant := storage.OAuthGrant{
		ID:        grantID,
		UserID:    userID,
		ClientID:  req.ClientID,
		Resource:  req.Resource,
		Scopes:    req.Scopes,
		CreatedAt: nowText,
	}
	codeRow := storage.OAuthCode{
		CodeHash:      hashToken(code),
		GrantID:       grantID,
		AuthRequestID: requestID,
		ClientID:      req.ClientID,
		RedirectURI:   req.RedirectURI,
		CodeChallenge: req.CodeChallenge,
		CreatedAt:     nowText,
		ExpiresAt:     formatTime(now.Add(codeTTL)),
	}
	if err := s.store.DecideOAuthAuthRequest(ctx, requestID, userID, nowText); err != nil {
		return "", mapDecisionErr(err)
	}
	if err := s.store.ApproveOAuthRequest(ctx, requestID, userID, grant, codeRow, nowText, audit); err != nil {
		return "", mapDecisionErr(err)
	}

	params := url.Values{}
	params.Set("code", code)
	if req.State != "" {
		params.Set("state", req.State)
	}
	return s.responseURL(req.RedirectURI, params)
}

// mapDecisionErr converts a storage "not decidable" race into the generic
// not-found error the browser sees.
func mapDecisionErr(err error) error {
	if errors.Is(err, storage.ErrOAuthRequestNotDecidable) {
		return ErrRequestNotFound
	}
	return err
}

// splitScopes parses the stored space-delimited scope string.
func splitScopes(s string) []string { return strings.Fields(s) }
