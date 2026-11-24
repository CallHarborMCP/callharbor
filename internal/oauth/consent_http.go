package oauth

import (
	"errors"
	"net/http"
)

// DecideFunc resolves a consent decision for the signed-in user. It is the
// seam chunk C fills with the authenticated session principal, so this
// package never reads cookies or sessions itself.
type DecideFunc func(r *http.Request) (userID string, ok bool)

// ServeDecision is an HTTP handler for POST /oauth/consent/decision. It takes
// the request ID and approve flag as form values, binds the decision to the
// user from the DecideFunc, and answers with the redirect URL the browser
// should follow. It is intentionally not mounted by Register: chunk C mounts
// the JSON API that wraps Decide with CSRF and session checks.
func (s *Server) ServeDecision(user DecideFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if err := r.ParseForm(); err != nil {
			writeNoStore(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		userID, ok := user(r)
		if !ok {
			writeNoStore(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
			return
		}
		approve := r.Form.Get("approve") == "true"
		target, err := s.Decide(r.Context(), r.Form.Get("request"), userID, approve)
		switch {
		case errors.Is(err, ErrRequestNotFound):
			writeNoStore(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		case err != nil:
			writeNoStore(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		default:
			writeNoStore(w, http.StatusOK, map[string]string{"redirect_url": target})
		}
	}
}
