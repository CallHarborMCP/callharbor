package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/CallHarborMCP/callharbor/internal/service"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephonyprofile"
)

// maxTelephonyBodyBytes bounds a telephony write. The two secret fields are
// each capped at 4096 bytes, so this leaves room for the other fields.
const maxTelephonyBodyBytes = 32 << 10

// telephonyWriteBody is the TelephonyProfileWrite request schema. Unknown
// fields are rejected. webhook_public_key is a *string so an explicit JSON null
// (removal on update) is distinct from an omitted or empty value (keep).
type telephonyWriteBody struct {
	Name         string `json:"name"`
	CallerNumber string `json:"caller_number"`
	ConnectionID string `json:"connection_id"`
	APIKey       string `json:"api_key"`
	// WebhookRaw keeps the raw value so "absent" and "null" can be told apart.
	WebhookRaw json.RawMessage `json:"webhook_public_key"`
}

// telephonyProfileBody is the TelephonyProfile response schema. It carries no
// secret reference. connection_id is returned to the owner only, so the owner
// can read back what they set. A granted view omits it.
type telephonyProfileBody struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Provider           string `json:"provider"`
	CallerNumber       string `json:"caller_number"`
	ConnectionID       string `json:"connection_id,omitempty"`
	ConnectivityStatus string `json:"connectivity_status"`
	ConnectivityReason string `json:"connectivity_reason"`
	Access             string `json:"access"`
}

func telephonyBody(v telephonyprofile.View) telephonyProfileBody {
	body := telephonyProfileBody{
		ID: v.ID, Name: v.Name, Provider: v.Provider, CallerNumber: v.CallerNumber,
		ConnectivityStatus: v.ConnectivityStatus, ConnectivityReason: v.ConnectivityReason,
		Access: v.Access,
	}
	if v.Access == storage.TelephonyAccessOwner {
		body.ConnectionID = v.ConnectionID
	}
	return body
}

// telephonyCaller resolves the signed-in session for a telephony route.
// Mutations also require the CSRF token bound to that session. On failure it
// has already written the error envelope.
func (h *Handler) telephonyCaller(w http.ResponseWriter, r *http.Request, mutating bool) (telephonyprofile.Actor, bool) {
	p, ok := h.auth.ResolveSession(r)
	if !ok {
		h.fail(w, service.AsErrorWith(service.CodeUnauthenticated, "authentication required"))
		return telephonyprofile.Actor{}, false
	}
	if mutating && !h.auth.CSRFValid(r, p) {
		h.fail(w, service.AsErrorWith(service.CodeForbidden, "csrf token invalid"))
		return telephonyprofile.Actor{}, false
	}
	return telephonyprofile.Principal(p), true
}

// decodeTelephonyWrite reads a strict JSON write body. On failure it has
// already written the error envelope.
func (h *Handler) decodeTelephonyWrite(w http.ResponseWriter, r *http.Request) (telephonyprofile.Input, bool) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		h.fail(w, service.AsErrorWith(service.CodeInvalidInput, "request body must be application/json"))
		return telephonyprofile.Input{}, false
	}
	var b telephonyWriteBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTelephonyBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil || !atJSONEnd(dec) {
		// One body is one JSON value. Anything after it, even whitespace-separated
		// junk such as a stray closing brace or a second object, is rejected.
		h.fail(w, service.AsErrorWith(service.CodeInvalidInput, "invalid JSON body"))
		return telephonyprofile.Input{}, false
	}
	in := telephonyprofile.Input{
		Name: b.Name, CallerNumber: b.CallerNumber, ConnectionID: b.ConnectionID,
		APIKey: b.APIKey,
	}
	switch raw := strings.TrimSpace(string(b.WebhookRaw)); raw {
	case "":
		// Absent: keep the stored webhook key.
	case "null":
		in.ClearWebhookPublicKey = true
	default:
		var key string
		if err := json.Unmarshal(b.WebhookRaw, &key); err != nil {
			h.fail(w, service.AsErrorWith(service.CodeInvalidInput, "invalid JSON body"))
			return telephonyprofile.Input{}, false
		}
		in.WebhookPublicKey = key
	}
	return in, true
}

// atJSONEnd reports whether nothing follows the decoded value but whitespace.
// Any further token, including a stray closing bracket, is trailing content.
func atJSONEnd(dec *json.Decoder) bool {
	_, err := dec.Token()
	return errors.Is(err, io.EOF)
}

func (h *Handler) listTelephonyProfiles(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.telephonyCaller(w, r, false)
	if !ok {
		return
	}
	views, err := h.tel.List(r.Context(), actor)
	if err != nil {
		h.failTelephony(w, err)
		return
	}
	out := make([]telephonyProfileBody, 0, len(views))
	for _, v := range views {
		out = append(out, telephonyBody(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"telephony_profiles": out})
}

func (h *Handler) getTelephonyProfile(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.telephonyCaller(w, r, false)
	if !ok {
		return
	}
	v, err := h.tel.Get(r.Context(), actor, r.PathValue("profile_id"))
	if err != nil {
		h.failTelephony(w, err)
		return
	}
	writeJSON(w, http.StatusOK, telephonyBody(v))
}

func (h *Handler) createTelephonyProfile(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.telephonyCaller(w, r, true)
	if !ok {
		return
	}
	in, ok := h.decodeTelephonyWrite(w, r)
	if !ok {
		return
	}
	v, err := h.tel.Create(r.Context(), actor, in)
	if err != nil {
		h.failTelephony(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, telephonyBody(v))
}

func (h *Handler) updateTelephonyProfile(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.telephonyCaller(w, r, true)
	if !ok {
		return
	}
	in, ok := h.decodeTelephonyWrite(w, r)
	if !ok {
		return
	}
	v, err := h.tel.Update(r.Context(), actor, r.PathValue("profile_id"), in)
	if err != nil {
		h.failTelephony(w, err)
		return
	}
	writeJSON(w, http.StatusOK, telephonyBody(v))
}

func (h *Handler) deleteTelephonyProfile(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.telephonyCaller(w, r, true)
	if !ok {
		return
	}
	if err := h.tel.Delete(r.Context(), actor, r.PathValue("profile_id")); err != nil {
		h.failTelephony(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateTelephonyProfile runs the read-only check. It issues GET requests
// only and never places a call.
func (h *Handler) validateTelephonyProfile(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.telephonyCaller(w, r, true)
	if !ok {
		return
	}
	res, err := h.tel.Validate(r.Context(), actor, r.PathValue("profile_id"))
	if err != nil {
		h.failTelephony(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) listTelephonyGrants(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.telephonyCaller(w, r, false)
	if !ok {
		return
	}
	grants, err := h.tel.ListGrants(r.Context(), actor, r.PathValue("profile_id"))
	if err != nil {
		h.failTelephony(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": grants})
}

func (h *Handler) grantTelephonyProfile(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.telephonyCaller(w, r, true)
	if !ok {
		return
	}
	if _, err := h.tel.Grant(r.Context(), actor, r.PathValue("profile_id"), r.PathValue("user_id")); err != nil {
		h.failTelephony(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) revokeTelephonyProfile(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.telephonyCaller(w, r, true)
	if !ok {
		return
	}
	if err := h.tel.Revoke(r.Context(), actor, r.PathValue("profile_id"), r.PathValue("user_id")); err != nil {
		h.failTelephony(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// failTelephony maps telephony sentinels to the shared envelope. Messages are
// fixed, caller-safe strings. Provider text and secret material never reach
// this layer.
func (h *Handler) failTelephony(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, telephonyprofile.ErrSecretsDown):
		// The envelope code set has no unavailable code, so this is the
		// internal code at status 503. The cause is logged, never rendered.
		if h.logger != nil {
			h.logger.Error("request failed", "transport", "rest", "code", "internal", "cause", err.Error())
		}
		writeJSON(w, http.StatusServiceUnavailable, service.Envelope{Error: service.EnvelopeDetail{
			Code: string(service.CodeInternal), Message: "secrets unavailable",
		}})
	case errors.Is(err, telephonyprofile.ErrNotFound):
		h.fail(w, service.AsErrorWith(service.CodeNotFound, "telephony profile not found"))
	case errors.Is(err, telephonyprofile.ErrForbidden):
		h.fail(w, service.AsErrorWith(service.CodeForbidden, "forbidden"))
	case errors.Is(err, telephonyprofile.ErrInvalidInput):
		h.fail(w, service.AsErrorWith(service.CodeInvalidInput, invalidMessage(err)))
	case errors.Is(err, telephonyprofile.ErrConflict):
		h.fail(w, service.AsErrorWith(service.CodeConflict, "telephony profile name already exists"))
	case errors.Is(err, telephonyprofile.ErrInUse):
		h.fail(w, service.AsErrorWith(service.CodeConflict, "telephony profile is referenced by calls"))
	case errors.Is(err, telephonyprofile.ErrValidationRace):
		h.fail(w, service.AsErrorWith(service.CodeConflict, "telephony profile changed during validation"))
	default:
		h.fail(w, err)
	}
}

// invalidMessage returns the caller-safe text of an invalid-input error. The
// package prefix is dropped; the field name that follows is kept.
func invalidMessage(err error) string {
	msg := strings.TrimPrefix(err.Error(), "telephonyprofile: ")
	if msg == "" {
		return "invalid input"
	}
	return msg
}
