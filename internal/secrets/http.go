package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

const maxSecretBody = 8 << 10

// Lister reads secret metadata. It never returns ciphertext or plaintext.
type Lister interface {
	ListSecrets(ctx context.Context, ownerUserID string) ([]storage.SecretMeta, error)
}

// Writer persists an encrypted secret for an owner and returns its id.
type Writer interface {
	Put(ctx context.Context, owner, name, purpose string, plaintext []byte) (string, error)
}

// Handler serves the write-only secrets API. There is deliberately no route
// that returns a secret value: plaintext is accepted once on create and never
// read back.
type Handler struct {
	auth   *identity.Handler
	writer Writer
	lister Lister
}

// NewHandler constructs the secrets API handler.
func NewHandler(auth *identity.Handler, writer Writer, lister Lister) *Handler {
	return &Handler{auth: auth, writer: writer, lister: lister}
}

// Register mounts the secrets routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/secrets", h.list)
	mux.HandleFunc("POST /api/secrets", h.create)
}

type createInput struct {
	Name      string `json:"name"`
	Purpose   string `json:"purpose"`
	Plaintext string `json:"value"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	p, ok := h.auth.AuthenticateMutating(w, r)
	if !ok {
		return
	}
	var in createInput
	if !decodeSecret(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 128 || !validPurposes[in.Purpose] {
		writeError(w, http.StatusBadRequest, "invalid_input")
		return
	}
	id, err := h.writer.Put(r.Context(), p.User.ID, name, in.Purpose, []byte(in.Plaintext))
	if err != nil {
		writeSecretError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": name, "purpose": in.Purpose})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := h.auth.Authenticate(w, r)
	if !ok {
		return
	}
	rows, err := h.lister.ListSecrets(r.Context(), p.User.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		out = append(out, map[string]any{
			"id":         m.ID,
			"name":       m.Name,
			"purpose":    m.Purpose,
			"created_at": m.CreatedAt,
			"rotated_at": nullableView(m.RotatedAt),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": out})
}

func nullableView(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func decodeSecret(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSecretBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}

func writeSecretError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidSecret):
		writeError(w, http.StatusBadRequest, "invalid_input")
	case errors.Is(err, ErrNoMasterKey):
		writeError(w, http.StatusServiceUnavailable, "secrets_unavailable")
	case errors.Is(err, storage.ErrConflict):
		writeError(w, http.StatusConflict, "conflict")
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
