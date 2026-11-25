package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// Authenticator resolves the signed-in user for a request. It writes the 401
// response itself and returns false when there is no session. The identity
// package satisfies it through a small adapter, so this package never reads
// cookies or sessions.
type Authenticator func(w http.ResponseWriter, r *http.Request) (userID string, ok bool)

// consentAPI holds the session adapters for the /api/oauth routes.
type consentAPI struct {
	s        *Server
	read     Authenticator
	mutating Authenticator
}

// RegisterConsentAPI mounts the signed-in consent and grant endpoints.
// authenticate checks a session; authenticateMutating also checks CSRF.
func (s *Server) RegisterConsentAPI(mux *http.ServeMux, authenticate, authenticateMutating Authenticator) {
	api := &consentAPI{s: s, read: authenticate, mutating: authenticateMutating}
	mux.HandleFunc("GET /api/oauth/requests/{id}", api.getRequest)
	mux.HandleFunc("POST /api/oauth/requests/{id}/decision", api.decide)
	mux.HandleFunc("GET /api/oauth/grants", api.listGrants)
	mux.HandleFunc("DELETE /api/oauth/grants/{id}", api.revokeGrant)
}

// apiError writes the identity-style error shape {"error":{"code":...}}.
func apiError(w http.ResponseWriter, status int, code string) {
	writeNoStore(w, status, map[string]any{"error": map[string]string{"code": code}})
}

// verifiedHost returns the domain that identifies a client on the consent
// screen. Only the host of a metadata-document client_id counts: the server
// fetched the document from that host. client_uri is self-declared, so it is
// never used as identity. A dynamically registered client has no verified
// domain.
func verifiedHost(c Client) string {
	if !c.MetadataDocument {
		return ""
	}
	u, err := url.Parse(c.ID)
	if err != nil {
		return ""
	}
	return u.Host
}

// getRequest returns the consent view of one pending request. Unknown,
// expired and decided requests all return 404.
func (a *consentAPI) getRequest(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.read(w, r); !ok {
		return
	}
	ctx := r.Context()
	req, err := a.s.LoadConsentRequest(ctx, r.PathValue("id"))
	if errors.Is(err, ErrRequestNotFound) {
		apiError(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal")
		return
	}
	client, err := a.s.clients.ResolveClient(ctx, req.ClientID)
	if err != nil {
		apiError(w, http.StatusNotFound, "not_found")
		return
	}
	redirect, err := a.redirectFor(ctx, r.PathValue("id"))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeNoStore(w, http.StatusOK, map[string]any{
		"id":                req.ID,
		"client_id":         req.ClientID,
		"client_name":       client.Name,
		"client_uri":        client.URI,
		"client_host":       verifiedHost(client),
		"metadata_document": client.MetadataDocument,
		"redirect_uri":      redirect,
		"scopes":            req.Scopes,
		"resource":          req.Resource,
		"expires_at":        req.ExpiresAt.UTC().Format(timeLayout),
	})
}

// redirectFor returns the registered redirect_uri of a pending request so the
// consent screen can show where the answer goes.
func (a *consentAPI) redirectFor(ctx context.Context, id string) (string, error) {
	row, err := a.s.store.GetOAuthAuthRequest(ctx, id)
	if err != nil {
		return "", err
	}
	return row.RedirectURI, nil
}

type decisionBody struct {
	Approve *bool `json:"approve"`
}

// decide records the signed-in user's decision and returns the redirect URL.
func (a *consentAPI) decide(w http.ResponseWriter, r *http.Request) {
	userID, ok := a.mutating(w, r)
	if !ok {
		return
	}
	var in decisionBody
	if !decodeJSON(w, r, &in) || in.Approve == nil {
		apiError(w, http.StatusBadRequest, "invalid_input")
		return
	}
	target, err := a.s.Decide(r.Context(), r.PathValue("id"), userID, *in.Approve)
	switch {
	case errors.Is(err, ErrRequestNotFound):
		apiError(w, http.StatusNotFound, "not_found")
	case err != nil:
		apiError(w, http.StatusInternalServerError, "internal")
	default:
		writeNoStore(w, http.StatusOK, map[string]string{"redirect_url": target})
	}
}

// listGrants returns the caller's live grants only.
func (a *consentAPI) listGrants(w http.ResponseWriter, r *http.Request) {
	userID, ok := a.read(w, r)
	if !ok {
		return
	}
	grants, err := a.s.store.ListOAuthGrants(r.Context(), userID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]map[string]any, 0, len(grants))
	for _, g := range grants {
		out = append(out, map[string]any{
			"id":           g.ID,
			"client_id":    g.ClientID,
			"client_name":  g.ClientName,
			"scopes":       splitScopes(g.Scopes),
			"created_at":   g.CreatedAt,
			"last_used_at": nullableText(g.LastUsedAt),
		})
	}
	writeNoStore(w, http.StatusOK, map[string]any{"grants": out})
}

// revokeGrant revokes the caller's own grant family. Another user's grant is
// reported as 404, so grant IDs cannot be probed across accounts.
func (a *consentAPI) revokeGrant(w http.ResponseWriter, r *http.Request) {
	userID, ok := a.mutating(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	g, err := a.s.store.GetOAuthGrantForUser(ctx, userID, id)
	if errors.Is(err, storage.ErrNotFound) {
		apiError(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal")
		return
	}
	if g.RevokedAt != "" {
		apiError(w, http.StatusNotFound, "not_found")
		return
	}
	now := formatTime(a.s.clock())
	err = a.s.store.RevokeOAuthGrant(ctx, g.ID, now, storage.AuditEvent{
		OccurredAt:   now,
		ActorUserID:  userID,
		ActorRole:    "user",
		TargetUserID: userID,
		Action:       "oauth.grant_revoked",
		ResourceType: "oauth_grant",
		ResourceID:   g.ID,
		Outcome:      "success",
		DetailJSON:   "{}",
	})
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// nullableText maps an empty stored timestamp to JSON null.
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// decodeJSON reads a small JSON object, requiring application/json and
// rejecting unknown fields, matching identity's strictness for its own API.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	mt := r.Header.Get("Content-Type")
	if !strings.HasPrefix(mt, "application/json") {
		apiError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFormBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}
