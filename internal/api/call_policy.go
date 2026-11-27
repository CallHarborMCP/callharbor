package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/CallHarborMCP/callharbor/internal/service"
)

// maxCallPolicyBodyBytes bounds a per-user policy write. A prefix list is short
// and the body is a few fields, so anything larger is refused.
const maxCallPolicyBodyBytes = 16 << 10

// callPolicyBody is the strict JSON body of PUT /api/v1/users/{user_id}/call-policy.
// Unknown fields are refused; a null numeric field inherits the installation.
type callPolicyBody struct {
	MaxConcurrentCalls *int64   `json:"max_concurrent_calls"`
	MaxCallsPerHour    *int64   `json:"max_calls_per_hour"`
	MaxDurationSeconds *int64   `json:"max_duration_seconds"`
	MaxCostAmount      *string  `json:"max_cost_amount"`
	AllowPrefixes      []string `json:"allow_prefixes"`
	DenyPrefixes       []string `json:"deny_prefixes"`
}

// getUserCallPolicy serves GET /api/v1/users/{user_id}/call-policy to an
// administrator. A non-admin session gets 403 before any read.
func (h *Handler) getUserCallPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r, false)
	if !ok {
		return
	}
	view, err := h.svc.GetUserCallPolicyForAdmin(r.Context(), actor, r.PathValue("user_id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// putUserCallPolicy serves PUT /api/v1/users/{user_id}/call-policy. It needs the
// CSRF token, an administrator, and a strict body. The write is audited.
func (h *Handler) putUserCallPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r, true)
	if !ok {
		return
	}
	in, ok := h.decodeCallPolicy(w, r)
	if !ok {
		return
	}
	view, err := h.svc.PutUserCallPolicy(r.Context(), actor, r.PathValue("user_id"), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// getOwnCallPolicy serves GET /api/v1/call-policy: the caller's own effective
// policy. It never shows another user's overrides.
func (h *Handler) getOwnCallPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(w, r)
	if !ok {
		return
	}
	eff, err := h.svc.GetOwnCallPolicy(r.Context(), actor)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, eff)
}

// adminActor resolves the session and returns the actor with its administrator
// flag. The role comes from the session principal, never from input. A mutation
// also needs the CSRF token. On failure it has written the error envelope.
func (h *Handler) adminActor(w http.ResponseWriter, r *http.Request, mutating bool) (service.Actor, bool) {
	p, ok := h.auth.ResolveSession(r)
	if !ok {
		h.fail(w, service.AsErrorWith(service.CodeUnauthenticated, "authentication required"))
		return service.Actor{}, false
	}
	if mutating && !h.auth.CSRFValid(r, p) {
		h.fail(w, service.AsErrorWith(service.CodeForbidden, "csrf token invalid"))
		return service.Actor{}, false
	}
	return service.Actor{
		UserID: p.User.ID,
		Scopes: sessionScopes,
		Admin:  p.User.Role == "admin",
	}, true
}

// decodeCallPolicy reads the strict JSON body. On failure it has written the
// error envelope.
func (h *Handler) decodeCallPolicy(w http.ResponseWriter, r *http.Request) (service.UserCallPolicyInput, bool) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		h.fail(w, service.AsErrorWith(service.CodeInvalidInput, "request body must be application/json"))
		return service.UserCallPolicyInput{}, false
	}
	var b callPolicyBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCallPolicyBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil || !atJSONEnd(dec) {
		h.fail(w, service.AsErrorWith(service.CodeInvalidInput, "invalid JSON body"))
		return service.UserCallPolicyInput{}, false
	}
	return service.UserCallPolicyInput{
		MaxConcurrentCalls: b.MaxConcurrentCalls,
		MaxCallsPerHour:    b.MaxCallsPerHour,
		MaxDurationSeconds: b.MaxDurationSeconds,
		MaxCostAmount:      b.MaxCostAmount,
		AllowPrefixes:      b.AllowPrefixes,
		DenyPrefixes:       b.DenyPrefixes,
	}, true
}
