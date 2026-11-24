package aiprofile

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

const maxBodyBytes = 32 << 10

// Store is the owner-scoped AI profile repository.
type Store interface {
	CreateAIProfile(ctx context.Context, p storage.AIProfile) error
	GetAIProfile(ctx context.Context, ownerUserID, id string) (storage.AIProfile, error)
	ListAIProfiles(ctx context.Context, ownerUserID string) ([]storage.AIProfile, error)
	UpdateAIProfile(ctx context.Context, p storage.AIProfile) error
}

// Handler serves per-user AI profile CRUD. Every route is scoped to the
// signed-in owner; another user's profile is reported as not found.
type Handler struct {
	auth  *identity.Handler
	store Store
	now   func() time.Time
}

func NewHandler(auth *identity.Handler, store Store, now func() time.Time) *Handler {
	return &Handler{auth: auth, store: store, now: now}
}

// Register mounts the profile routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ai-profiles", h.list)
	mux.HandleFunc("POST /api/ai-profiles", h.create)
	mux.HandleFunc("GET /api/ai-profiles/{id}", h.get)
	mux.HandleFunc("PUT /api/ai-profiles/{id}", h.update)
}

type profileInput struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Protocol     string            `json:"protocol"`
	EndpointURL  string            `json:"endpoint_url"`
	Model        string            `json:"model"`
	Voice        string            `json:"voice"`
	Instructions string            `json:"instructions"`
	Headers      map[string]string `json:"headers"`
	APISecretID  string            `json:"api_secret_id"`
}

func view(p storage.AIProfile) map[string]any {
	return map[string]any{
		"id":            p.ID,
		"name":          p.Name,
		"protocol":      string(p.Protocol),
		"endpoint_url":  p.EndpointURL,
		"model":         p.Model,
		"voice":         p.Voice,
		"instructions":  p.Instructions,
		"headers":       p.Headers,
		"api_secret_id": p.APISecretID,
		"created_at":    p.CreatedAt,
		"updated_at":    p.UpdatedAt,
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := h.auth.Authenticate(w, r)
	if !ok {
		return
	}
	profiles, err := h.store.ListAIProfiles(r.Context(), p.User.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]map[string]any, 0, len(profiles))
	for _, prof := range profiles {
		out = append(out, view(prof))
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := h.auth.Authenticate(w, r)
	if !ok {
		return
	}
	prof, err := h.store.GetAIProfile(r.Context(), p.User.ID, r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view(prof))
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	p, ok := h.auth.AuthenticateMutating(w, r)
	if !ok {
		return
	}
	in, ok := decode(w, r)
	if !ok {
		return
	}
	now := h.now().UTC().Format("2006-01-02T15:04:05.000Z")
	prof := toProfile(in, p.User.ID)
	prof.CreatedAt, prof.UpdatedAt = now, now
	if err := h.store.CreateAIProfile(r.Context(), prof); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view(prof))
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	p, ok := h.auth.AuthenticateMutating(w, r)
	if !ok {
		return
	}
	in, ok := decode(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	existing, err := h.store.GetAIProfile(r.Context(), p.User.ID, id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	prof := toProfile(in, p.User.ID)
	prof.ID = id
	prof.CreatedAt = existing.CreatedAt
	prof.UpdatedAt = h.now().UTC().Format("2006-01-02T15:04:05.000Z")
	if err := h.store.UpdateAIProfile(r.Context(), prof); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view(prof))
}

func toProfile(in profileInput, owner string) storage.AIProfile {
	return storage.AIProfile{
		ID:           in.ID,
		OwnerUserID:  owner,
		Name:         strings.TrimSpace(in.Name),
		Protocol:     storage.AIProtocol(in.Protocol),
		EndpointURL:  strings.TrimSpace(in.EndpointURL),
		Model:        strings.TrimSpace(in.Model),
		Voice:        strings.TrimSpace(in.Voice),
		Instructions: in.Instructions,
		Headers:      in.Headers,
		APISecretID:  in.APISecretID,
	}
}

func decode(w http.ResponseWriter, r *http.Request) (profileInput, bool) {
	var in profileInput
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return in, false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return in, false
	}
	return in, true
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, storage.ErrAIProfileNotFound):
		writeError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, storage.ErrInvalidAIProfile):
		writeError(w, http.StatusBadRequest, "invalid_input")
	default:
		writeError(w, http.StatusInternalServerError, "internal")
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code}})
}
