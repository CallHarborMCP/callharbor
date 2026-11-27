package api

import (
	"net/http"
)

// hangupCall ends one call on request. Hangup is a state change, so it needs a
// session and the CSRF token, as every other session POST does. It needs no
// Idempotency-Key: a repeated hangup is safe, because the call's state decides
// what happens and the Telnyx command id is stable. The owner or an
// administrator may end a call; the service refuses everyone else with
// not_found.
func (h *Handler) hangupCall(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.adminActor(w, r, true)
	if !ok {
		return
	}
	res, err := h.svc.HangupCall(r.Context(), actor, r.PathValue("call_id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
