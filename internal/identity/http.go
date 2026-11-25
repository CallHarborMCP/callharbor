package identity

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

const (
	SessionCookieName = "callharbor_session"
	CSRFHeader        = "X-CSRF-Token"
	maxBodyBytes      = 16 << 10
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Register mounts identity routes on mux. Mutating routes require a valid
// session and a matching CSRF header.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/setup", h.setupStatus)
	mux.HandleFunc("POST /api/setup", h.setup)
	mux.HandleFunc("POST /api/session", h.login)
	mux.HandleFunc("GET /api/session", h.me)
	mux.HandleFunc("DELETE /api/session", h.logout)
	mux.HandleFunc("GET /api/users", h.listUsers)
	mux.HandleFunc("POST /api/users", h.createUser)
	mux.HandleFunc("GET /api/users/{id}", h.getUser)
	mux.HandleFunc("PATCH /api/users/{id}", h.updateUser)
	mux.HandleFunc("PUT /api/users/{id}/password", h.resetPassword)
	mux.HandleFunc("GET /api/preferences/appearance", h.getAppearance)
	mux.HandleFunc("PUT /api/preferences/appearance", h.setAppearance)
}

func (h *Handler) getAppearance(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	a, err := h.svc.GetAppearance(r.Context(), p)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeOK(w, http.StatusOK, a)
}

func (h *Handler) setAppearance(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authenticateMutating(w, r)
	if !ok {
		return
	}
	var in Appearance
	if !decode(w, r, &in) {
		return
	}
	a, err := h.svc.SetAppearance(r.Context(), p, in)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeOK(w, http.StatusOK, a)
}

func (h *Handler) setupStatus(w http.ResponseWriter, r *http.Request) {
	required, err := h.svc.SetupRequired(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	writeOK(w, http.StatusOK, map[string]any{"setup_required": required})
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *Handler) setup(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !decode(w, r, &in) {
		return
	}
	u, err := h.svc.Setup(r.Context(), in.Username, in.Password)
	switch {
	case errors.Is(err, storage.ErrSetupComplete):
		writeError(w, http.StatusConflict, "setup_complete")
	case errors.Is(err, storage.ErrConflict):
		writeError(w, http.StatusConflict, "username_taken")
	case errors.Is(err, ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_input")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal")
	default:
		writeOK(w, http.StatusCreated, userView(u))
	}
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var in credentials
	if !decode(w, r, &in) {
		return
	}
	res, err := h.svc.Login(r.Context(), in.Username, in.Password, clientIP(r))
	switch {
	case errors.Is(err, ErrThrottled):
		w.Header().Set("Retry-After", "900")
		writeError(w, http.StatusTooManyRequests, "throttled")
	case errors.Is(err, ErrInvalidLogin), errors.Is(err, ErrInactive):
		writeError(w, http.StatusUnauthorized, "invalid_credentials")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal")
	default:
		http.SetCookie(w, sessionCookie(res.Token, res.ExpiresAt))
		writeOK(w, http.StatusOK, map[string]any{"user": userView(res.User), "csrf_token": res.CSRFToken})
	}
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	writeOK(w, http.StatusOK, map[string]any{"user": userView(p.User), "csrf_token": p.CSRFToken})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authenticateMutating(w, r)
	if !ok {
		return
	}
	if err := h.svc.Logout(r.Context(), p); err != nil {
		writeError(w, http.StatusInternalServerError, "internal")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listUsers(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	users, err := h.svc.ListUsers(r.Context(), p)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, userView(u))
	}
	writeOK(w, http.StatusOK, map[string]any{"users": out})
}

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

func (h *Handler) createUser(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authenticateMutating(w, r)
	if !ok {
		return
	}
	var in createUserRequest
	if !decode(w, r, &in) {
		return
	}
	u, err := h.svc.CreateUser(r.Context(), p, in.Username, in.Password, in.Role)
	if err != nil {
		if errors.Is(err, storage.ErrConflict) {
			writeError(w, http.StatusConflict, "username_taken")
			return
		}
		writeServiceError(w, err)
		return
	}
	writeOK(w, http.StatusCreated, userView(u))
}

func (h *Handler) getUser(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	u, err := h.svc.GetUser(r.Context(), p, r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeOK(w, http.StatusOK, userView(u))
}

type updateUserRequest struct {
	Role   *string `json:"role"`
	Active *bool   `json:"active"`
}

func (h *Handler) updateUser(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authenticateMutating(w, r)
	if !ok {
		return
	}
	var in updateUserRequest
	if !decode(w, r, &in) {
		return
	}
	err := h.svc.UpdateUser(r.Context(), p, r.PathValue("id"), in.Role, in.Active)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if errors.Is(err, storage.ErrLastAdmin) {
		writeError(w, http.StatusConflict, "last_admin")
		return
	}
	if err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type passwordRequest struct {
	Password string `json:"password"`
}

func (h *Handler) resetPassword(w http.ResponseWriter, r *http.Request) {
	p, ok := h.authenticateMutating(w, r)
	if !ok {
		return
	}
	var in passwordRequest
	if !decode(w, r, &in) {
		return
	}
	err := h.svc.ResetPassword(r.Context(), p, r.PathValue("id"), in.Password)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Authenticate resolves the signed-in principal for a request, writing a 401
// response and returning false when there is none. It is exported so sibling
// owner-scoped APIs share the identity session and CSRF rules.
func (h *Handler) Authenticate(w http.ResponseWriter, r *http.Request) (*Principal, bool) {
	return h.authenticate(w, r)
}

// AuthenticateMutating is Authenticate plus the CSRF check for writes.
func (h *Handler) AuthenticateMutating(w http.ResponseWriter, r *http.Request) (*Principal, bool) {
	return h.authenticateMutating(w, r)
}

// ResolveSession returns the signed-in principal for r without writing a
// response. ok is false when the session cookie is missing or not valid.
func (h *Handler) ResolveSession(r *http.Request) (*Principal, bool) {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return nil, false
	}
	p, err := h.svc.Authenticate(r.Context(), c.Value)
	if err != nil {
		return nil, false
	}
	return p, true
}

func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (*Principal, bool) {
	p, ok := h.ResolveSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated")
		return nil, false
	}
	return p, true
}

func (h *Handler) authenticateMutating(w http.ResponseWriter, r *http.Request) (*Principal, bool) {
	p, ok := h.authenticate(w, r)
	if !ok {
		return nil, false
	}
	if !csrfMatches(r.Header.Get(CSRFHeader), p.CSRFToken) {
		writeError(w, http.StatusForbidden, "csrf_invalid")
		return nil, false
	}
	return p, true
}

func csrfMatches(got, want string) bool {
	if got == "" || want == "" || len(got) != len(want) {
		return false
	}
	var diff byte
	for i := 0; i < len(got); i++ {
		diff |= got[i] ^ want[i]
	}
	return diff == 0
}

func sessionCookie(token string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name: SessionCookieName, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func userView(u storage.User) map[string]any {
	return map[string]any{
		"id":         u.ID,
		"username":   u.Username,
		"role":       u.Role,
		"active":     u.Active,
		"created_at": u.CreatedAt,
		"updated_at": u.UpdatedAt,
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "unauthenticated")
	case errors.Is(err, ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid_input")
	case errors.Is(err, storage.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found")
	default:
		writeError(w, http.StatusInternalServerError, "internal")
	}
}

func writeOK(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeOK(w, status, map[string]any{"error": map[string]string{"code": code}})
}
