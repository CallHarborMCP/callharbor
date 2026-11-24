package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestHTTPSetupStatusSessionAndLogout(t *testing.T) {
	mux, _, _ := newHTTP(t)
	rec := do(t, mux, http.MethodGet, "/api/setup", nil, nil, "")
	if rec.Code != http.StatusOK || !jsonBool(t, rec.Body.Bytes(), "setup_required") {
		t.Fatalf("setup status before setup: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, mux, http.MethodPost, "/api/setup", map[string]string{"username": "root", "password": pwAdmin}, nil, ""); rec.Code != http.StatusCreated {
		t.Fatalf("setup: %d", rec.Code)
	}
	rec = do(t, mux, http.MethodGet, "/api/setup", nil, nil, "")
	if jsonBool(t, rec.Body.Bytes(), "setup_required") {
		t.Fatal("setup still required after admin created")
	}

	if rec := do(t, mux, http.MethodGet, "/api/session", nil, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session without cookie: %d", rec.Code)
	}
	cookie, csrf := loginHTTP(t, mux, "root", pwAdmin)
	rec = do(t, mux, http.MethodGet, "/api/session", nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("session with cookie: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, mux, http.MethodDelete, "/api/session", nil, cookie, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodDelete, "/api/session", nil, cookie, csrf); rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodGet, "/api/session", nil, cookie, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session after logout: %d", rec.Code)
	}
}

func TestHTTPAdminUpdateAndPasswordReset(t *testing.T) {
	mux, _, store := newHTTP(t)
	if rec := do(t, mux, http.MethodPost, "/api/setup", map[string]string{"username": "root", "password": pwAdmin}, nil, ""); rec.Code != http.StatusCreated {
		t.Fatalf("setup: %d", rec.Code)
	}
	rootCookie, rootCSRF := loginHTTP(t, mux, "root", pwAdmin)
	if rec := do(t, mux, http.MethodPost, "/api/users", map[string]string{"username": "alice", "password": pwUser, "role": "user"}, rootCookie, rootCSRF); rec.Code != http.StatusCreated {
		t.Fatalf("create alice: %d", rec.Code)
	}
	aliceID := userID(t, store, "alice")
	rootID := userID(t, store, "root")

	if rec := do(t, mux, http.MethodPatch, "/api/users/"+aliceID, map[string]string{"role": "admin"}, rootCookie, rootCSRF); rec.Code != http.StatusNoContent {
		t.Fatalf("promote alice: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, mux, http.MethodPatch, "/api/users/"+rootID, map[string]bool{"active": false}, rootCookie, rootCSRF); rec.Code != http.StatusForbidden {
		t.Fatalf("self-deactivate: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPatch, "/api/users/"+aliceID, map[string]any{"role": "wizard"}, rootCookie, rootCSRF); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid role: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPatch, "/api/users/missing", map[string]string{"role": "user"}, rootCookie, rootCSRF); rec.Code != http.StatusNotFound {
		t.Fatalf("patch missing user: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPut, "/api/users/"+aliceID+"/password", map[string]string{"password": "brand new long password"}, rootCookie, rootCSRF); rec.Code != http.StatusNoContent {
		t.Fatalf("reset password: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, mux, http.MethodPut, "/api/users/missing/password", map[string]string{"password": "brand new long password"}, rootCookie, rootCSRF); rec.Code != http.StatusNotFound {
		t.Fatalf("reset missing user: %d", rec.Code)
	}
}

func TestHTTPSetupAndCreateUserErrorBranches(t *testing.T) {
	mux, svc, _ := newHTTP(t)
	ctx := context.Background()
	if rec := do(t, mux, http.MethodPost, "/api/setup", map[string]string{"username": "bad name!", "password": pwAdmin}, nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("setup invalid username: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPost, "/api/setup", map[string]string{"username": "root", "password": pwAdmin}, nil, ""); rec.Code != http.StatusCreated {
		t.Fatalf("setup: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPost, "/api/setup", map[string]string{"username": "again", "password": pwAdmin}, nil, ""); rec.Code != http.StatusConflict {
		t.Fatalf("second setup: %d", rec.Code)
	}
	cookie, csrf := loginHTTP(t, mux, "root", pwAdmin)
	if rec := do(t, mux, http.MethodPost, "/api/users", map[string]string{"username": "root", "password": pwUser, "role": "user"}, cookie, csrf); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate username: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPost, "/api/users", map[string]string{"username": "bob", "password": "short", "role": "user"}, cookie, csrf); rec.Code != http.StatusBadRequest {
		t.Fatalf("short password: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodGet, "/api/users", nil, cookie, ""); rec.Code != http.StatusOK {
		t.Fatalf("list users: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPut, "/api/preferences/appearance", Appearance{Theme: "dark", Preset: "console", Accent: "blue", Density: "compact"}, cookie, "wrong"); rec.Code != http.StatusForbidden {
		t.Fatalf("preferences wrong CSRF: %d", rec.Code)
	}
	if _, err := svc.SetAppearance(ctx, nil, DefaultAppearance); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("SetAppearance nil actor = %v", err)
	}
	if _, err := svc.GetAppearance(ctx, nil); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("GetAppearance nil actor = %v", err)
	}
}

func jsonBool(t *testing.T, body []byte, key string) bool {
	t.Helper()
	var out map[string]bool
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	return out[key]
}
