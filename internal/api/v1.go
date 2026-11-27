// Package api serves the versioned REST v1 API. Handlers authenticate the
// browser session, build a service.Actor from that session only, and call the
// shared service layer that the MCP transport also uses.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/service"
	"github.com/CallHarborMCP/callharbor/internal/telephonyprofile"
)

// sessionScopes are granted to a browser session on REST. A session is the
// full account, so it carries both scopes the service enforces.
var sessionScopes = []string{service.ScopeRead, service.ScopeCall}

// Route is one mounted REST v1 operation. Routes is the single source for the
// mux registration and for the OpenAPI drift test, so an undocumented route
// cannot be mounted without failing the build.
type Route struct {
	Method string
	Path   string
}

// Routes lists every implemented REST v1 operation.
var Routes = []Route{
	{Method: http.MethodGet, Path: "/api/v1/profiles"},
	{Method: http.MethodGet, Path: "/api/v1/calls"},
	{Method: http.MethodPost, Path: "/api/v1/calls"},
	{Method: http.MethodGet, Path: "/api/v1/calls/{call_id}"},
	{Method: http.MethodGet, Path: "/api/v1/calls/{call_id}/transcript"},
	{Method: http.MethodPost, Path: "/api/v1/calls/{call_id}/hangup"},
	{Method: http.MethodGet, Path: "/api/v1/telephony-profiles"},
	{Method: http.MethodPost, Path: "/api/v1/telephony-profiles"},
	{Method: http.MethodGet, Path: "/api/v1/telephony-profiles/{profile_id}"},
	{Method: http.MethodPut, Path: "/api/v1/telephony-profiles/{profile_id}"},
	{Method: http.MethodDelete, Path: "/api/v1/telephony-profiles/{profile_id}"},
	{Method: http.MethodPost, Path: "/api/v1/telephony-profiles/{profile_id}/validate"},
	{Method: http.MethodGet, Path: "/api/v1/telephony-profiles/{profile_id}/grants"},
	{Method: http.MethodPut, Path: "/api/v1/telephony-profiles/{profile_id}/grants/{user_id}"},
	{Method: http.MethodDelete, Path: "/api/v1/telephony-profiles/{profile_id}/grants/{user_id}"},
	{Method: http.MethodGet, Path: "/api/v1/call-policy"},
	{Method: http.MethodGet, Path: "/api/v1/users/{user_id}/call-policy"},
	{Method: http.MethodPut, Path: "/api/v1/users/{user_id}/call-policy"},
}

// PlannedRoutes lists REST operations documented in api/openapi.json with
// x-callharbor-availability: planned. They are never mounted, so a request to
// one returns 404 from the mux. Routes holds only available paths; the two
// lists are disjoint, and the tests enforce that.
var PlannedRoutes = []Route{
	{Method: http.MethodGet, Path: "/api/v1/calls/{call_id}/recording"},
	{Method: http.MethodGet, Path: "/api/v1/calls/{call_id}/recording/audio"},
}

// Handler serves the /api/v1 routes.
type Handler struct {
	auth   *identity.Handler
	svc    *service.Service
	tel    *telephonyprofile.Service
	logger *slog.Logger
}

// NewHandler returns the REST v1 handler. tel serves the telephony profile
// routes; logger receives server-side causes of internal errors and may be
// nil.
func NewHandler(auth *identity.Handler, svc *service.Service, tel *telephonyprofile.Service, logger *slog.Logger) *Handler {
	return &Handler{auth: auth, svc: svc, tel: tel, logger: logger}
}

// Register mounts the implemented v1 routes on mux, and a planned-route answer
// for each planned GET path. A planned GET is not implemented, so it gets a
// JSON 404 not_found in the shared envelope, never the mux's plain-text 404 and
// never the web UI. Other planned methods on paths that are not mounted fall
// to the mux's own 404 or 405 as before; a wrong method on a mounted path is
// 405 from the mux.
//
// The planned answers are derived from PlannedRoutes, so the list has one
// source. They grant nothing: each one is the same 404 an unknown call gets.
func (h *Handler) Register(mux *http.ServeMux) {
	handlers := map[string]http.HandlerFunc{
		"GET /api/v1/profiles":                   h.getProfiles,
		"GET /api/v1/calls":                      h.listCalls,
		"POST /api/v1/calls":                     h.createCall,
		"GET /api/v1/calls/{call_id}":            h.getCall,
		"GET /api/v1/calls/{call_id}/transcript": h.getCallTranscript,
		"POST /api/v1/calls/{call_id}/hangup":    h.hangupCall,

		"GET /api/v1/telephony-profiles":                                  h.listTelephonyProfiles,
		"POST /api/v1/telephony-profiles":                                 h.createTelephonyProfile,
		"GET /api/v1/telephony-profiles/{profile_id}":                     h.getTelephonyProfile,
		"PUT /api/v1/telephony-profiles/{profile_id}":                     h.updateTelephonyProfile,
		"DELETE /api/v1/telephony-profiles/{profile_id}":                  h.deleteTelephonyProfile,
		"POST /api/v1/telephony-profiles/{profile_id}/validate":           h.validateTelephonyProfile,
		"GET /api/v1/telephony-profiles/{profile_id}/grants":              h.listTelephonyGrants,
		"PUT /api/v1/telephony-profiles/{profile_id}/grants/{user_id}":    h.grantTelephonyProfile,
		"DELETE /api/v1/telephony-profiles/{profile_id}/grants/{user_id}": h.revokeTelephonyProfile,

		"GET /api/v1/call-policy":                 h.getOwnCallPolicy,
		"GET /api/v1/users/{user_id}/call-policy": h.getUserCallPolicy,
		"PUT /api/v1/users/{user_id}/call-policy": h.putUserCallPolicy,
	}
	for _, r := range Routes {
		pattern := r.Method + " " + r.Path
		fn, ok := handlers[pattern]
		if !ok {
			// A route in the table without a handler is a wiring bug.
			panic("api: no handler for " + pattern)
		}
		mux.HandleFunc(pattern, fn)
	}
	for _, r := range PlannedRoutes {
		if r.Method != http.MethodGet {
			continue
		}
		pattern := r.Method + " " + r.Path
		if _, mounted := handlers[pattern]; mounted {
			// The two lists are disjoint; this guards a future edit.
			panic("api: planned route is also mounted: " + pattern)
		}
		mux.HandleFunc(pattern, h.planned)
	}
}

// planned answers a documented but unimplemented operation. It applies the same
// session check as the implemented routes, so an anonymous request gets 401 as
// everywhere else on /api/v1. A signed-in request gets 404 not_found, the same
// answer as an unknown call, with no data and no side effect.
func (h *Handler) planned(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.actor(w, r); !ok {
		return
	}
	h.fail(w, service.AsErrorWith(service.CodeNotFound, "not found"))
}

// actor resolves the session and returns the REST actor. On failure it has
// already written a 401 error body.
func (h *Handler) actor(w http.ResponseWriter, r *http.Request) (service.Actor, bool) {
	p, ok := h.auth.ResolveSession(r)
	if !ok {
		h.fail(w, service.AsErrorWith(service.CodeUnauthenticated, "authentication required"))
		return service.Actor{}, false
	}
	return service.Actor{
		UserID: p.User.ID,
		Scopes: sessionScopes,
	}, true
}

func (h *Handler) getProfiles(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	res, err := h.svc.GetProfiles(r.Context(), actor)
	if err != nil {
		h.fail(w, err)
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
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < service.MinLimit {
			// 0 is the service's "not supplied" sentinel, so an explicit
			// limit=0 must be rejected here rather than silently defaulted.
			h.fail(w, service.AsErrorWith(service.CodeInvalidInput, "limit must be an integer from 1 to 100"))
			return
		}
		in.Limit = n
	}
	res, err := h.svc.ListCalls(r.Context(), actor, in)
	if err != nil {
		h.fail(w, err)
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
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// getCallTranscript serves one page of a call's transcript. The cursor is
// opaque and bounded by the service, which rejects an oversized or malformed
// token as invalid_input before it reads the store.
func (h *Handler) getCallTranscript(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	in := service.GetTranscriptInput{CallID: r.PathValue("call_id"), Cursor: r.URL.Query().Get("cursor")}
	res, err := h.svc.GetTranscript(r.Context(), actor, in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// fail logs the server-side cause of an internal error and writes the shared
// envelope. The envelope never carries the cause.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	service.LogCause(h.logger, "rest", err)
	env := service.EnvelopeOf(err)
	writeJSON(w, service.AsError(err).Code.HTTPStatus(), env)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
