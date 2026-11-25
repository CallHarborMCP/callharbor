// Package api serves the versioned REST v1 API. Handlers authenticate the
// browser session, build a service.Actor from that session only, and call the
// shared service layer that the MCP transport also uses.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/service"
)

// sessionScopes are granted to a browser session on REST. A session is the
// full account, so it carries both scopes the service enforces.
var sessionScopes = []string{service.ScopeRead, service.ScopeCall}

// Handler serves the /api/v1 routes.
type Handler struct {
	auth *identity.Handler
	svc  *service.Service
}

// NewHandler returns the REST v1 handler.
func NewHandler(auth *identity.Handler, svc *service.Service) *Handler {
	return &Handler{auth: auth, svc: svc}
}

// Register mounts the v1 routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/profiles", h.getProfiles)
	mux.HandleFunc("GET /api/v1/calls", h.listCalls)
	mux.HandleFunc("GET /api/v1/calls/{call_id}", h.getCall)
}

// actor resolves the session and returns the REST actor. On failure it has
// already written a 401 error body.
func (h *Handler) actor(w http.ResponseWriter, r *http.Request) (service.Actor, bool) {
	p, ok := h.auth.ResolveSession(r)
	if !ok {
		writeServiceError(w, service.AsErrorWith(service.CodeUnauthenticated, "authentication required"))
		return service.Actor{}, false
	}
	return service.Actor{
		UserID:  p.User.ID,
		Scopes:  sessionScopes,
		Channel: service.ChannelREST,
	}, true
}

func (h *Handler) getProfiles(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	res, err := h.svc.GetProfiles(r.Context(), actor)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) listCalls(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	in := service.ListCallsInput{Cursor: q.Get("cursor"), Status: q.Get("status")}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			// 0 is the service's "not supplied" sentinel, so an explicit
			// limit=0 must be rejected here rather than silently defaulted.
			writeServiceError(w, service.AsErrorWith(service.CodeInvalidInput, "limit must be an integer from 1 to 100"))
			return
		}
		in.Limit = n
	}
	res, err := h.svc.ListCalls(r.Context(), actor, in)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) getCall(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	res, err := h.svc.GetCall(r.Context(), actor, r.PathValue("call_id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ErrorBody is the shared REST error envelope.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail carries the stable code and a caller-safe message.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeServiceError(w http.ResponseWriter, err error) {
	se := service.AsError(err)
	writeJSON(w, se.Code.HTTPStatus(), ErrorBody{Error: ErrorDetail{Code: string(se.Code), Message: se.Message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
