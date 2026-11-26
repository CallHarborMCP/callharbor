package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/CallHarborMCP/callharbor/internal/service"
)

// maxMakeCallBodyBytes bounds the request body. The brief limit is enforced
// separately and is configurable. This cap only stops an oversized body before
// it is parsed, so it sits above the largest brief the config allows.
const maxMakeCallBodyBytes = 300 << 10

// idempotencyHeader is the request header that carries the idempotency key.
const idempotencyHeader = "Idempotency-Key"

// makeCallBody is the CreateCallRequest schema. Unknown fields are rejected.
// Request-level fields map one to one onto service.MakeCallInput.
type makeCallBody struct {
	ProfileID       string   `json:"profile_id"`
	To              string   `json:"to"`
	Recipient       string   `json:"recipient"`
	Language        string   `json:"language"`
	Objective       string   `json:"objective"`
	Context         string   `json:"context"`
	Instructions    string   `json:"instructions"`
	Constraints     []string `json:"constraints"`
	Fallbacks       []string `json:"fallbacks"`
	SuccessCriteria []string `json:"success_criteria"`
	MaxDuration     *int     `json:"max_duration_seconds"`
	Record          bool     `json:"record"`
}

// createCall handles POST /api/v1/calls. It requires a signed-in session, the
// CSRF token bound to it, and an Idempotency-Key header. It returns 202 with
// the queued call. The dial happens later in the worker.
func (h *Handler) createCall(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.mutatingActor(w, r)
	if !ok {
		return
	}
	key := r.Header.Get(idempotencyHeader)
	if key == "" {
		h.fail(w, service.AsErrorWith(service.CodeInvalidInput, "idempotency_key_required: send the Idempotency-Key header"))
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		h.fail(w, service.AsErrorWith(service.CodeInvalidInput, "request body must be application/json"))
		return
	}
	var b makeCallBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMakeCallBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil || !atJSONEnd(dec) {
		h.fail(w, service.AsErrorWith(service.CodeInvalidInput, "invalid JSON body"))
		return
	}
	res, err := h.svc.MakeCall(r.Context(), actor, service.MakeCallInput{
		ProfileID: b.ProfileID, To: b.To, Recipient: b.Recipient, Language: b.Language,
		Objective: b.Objective, Context: b.Context, Instructions: b.Instructions,
		Constraints: b.Constraints, Fallbacks: b.Fallbacks, SuccessCriteria: b.SuccessCriteria,
		MaxDuration: b.MaxDuration, Record: b.Record, IdempotencyKey: key,
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// mutatingActor is the session check for a state-changing REST route. It
// requires the CSRF token bound to the session, like every telephony write.
func (h *Handler) mutatingActor(w http.ResponseWriter, r *http.Request) (service.Actor, bool) {
	p, ok := h.auth.ResolveSession(r)
	if !ok {
		h.fail(w, service.AsErrorWith(service.CodeUnauthenticated, "authentication required"))
		return service.Actor{}, false
	}
	if !h.auth.CSRFValid(r, p) {
		h.fail(w, service.AsErrorWith(service.CodeForbidden, "csrf token invalid"))
		return service.Actor{}, false
	}
	return service.Actor{UserID: p.User.ID, Scopes: sessionScopes}, true
}
