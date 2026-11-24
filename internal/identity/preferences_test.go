package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestAppearanceDefaultsWhenUnset(t *testing.T) {
	svc, _, _, sess := fixtureWithUsers(t)
	got, err := svc.GetAppearance(context.Background(), sess["alice"].p)
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultAppearance {
		t.Fatalf("default appearance = %+v", got)
	}
}

func TestAppearanceRoundTripsForOwnUser(t *testing.T) {
	svc, _, _, sess := fixtureWithUsers(t)
	ctx := context.Background()
	want := Appearance{Theme: "dark", Preset: "harbor", Accent: "teal", Density: "compact"}
	if _, err := svc.SetAppearance(ctx, sess["alice"].p, want); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetAppearance(ctx, sess["alice"].p)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestAppearanceRejectsValuesOutsideClosedSets(t *testing.T) {
	svc, _, _, sess := fixtureWithUsers(t)
	ctx := context.Background()
	bad := []Appearance{
		{Theme: "neon", Preset: "console", Accent: "blue", Density: "compact"},
		{Theme: "dark", Preset: "retro", Accent: "blue", Density: "compact"},
		{Theme: "dark", Preset: "console", Accent: "red", Density: "compact"},
		{Theme: "dark", Preset: "console", Accent: "blue", Density: "spacious"},
		{},
	}
	for _, in := range bad {
		if _, err := svc.SetAppearance(ctx, sess["alice"].p, in); err != ErrInvalidInput {
			t.Fatalf("SetAppearance(%+v) = %v, want ErrInvalidInput", in, err)
		}
	}
	got, _ := svc.GetAppearance(ctx, sess["alice"].p)
	if got != DefaultAppearance {
		t.Fatalf("rejected write changed stored value: %+v", got)
	}
}

func TestAppearanceIsIsolatedPerUser(t *testing.T) {
	svc, _, _, sess := fixtureWithUsers(t)
	ctx := context.Background()
	if _, err := svc.SetAppearance(ctx, sess["alice"].p, Appearance{Theme: "dark", Preset: "harbor", Accent: "violet", Density: "compact"}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetAppearance(ctx, sess["bob"].p)
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultAppearance {
		t.Fatalf("bob sees alice's preferences: %+v", got)
	}
}

func TestAppearanceHTTPRequiresSessionAndCSRF(t *testing.T) {
	mux, _, store := newHTTP(t)
	if rec := do(t, mux, http.MethodPost, "/api/setup", map[string]string{"username": "root", "password": pwAdmin}, nil, ""); rec.Code != http.StatusCreated {
		t.Fatalf("setup: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPost, "/api/users", map[string]string{"username": "alice", "password": pwUser, "role": "user"}, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("create user without session: %d", rec.Code)
	}
	rootCookie, rootCSRF := loginHTTP(t, mux, "root", pwAdmin)
	if rec := do(t, mux, http.MethodPost, "/api/users", map[string]string{"username": "alice", "password": pwUser, "role": "user"}, rootCookie, rootCSRF); rec.Code != http.StatusCreated {
		t.Fatalf("create user: %d %s", rec.Code, rec.Body.String())
	}
	cookie, csrf := loginHTTP(t, mux, "alice", pwUser)
	body := Appearance{Theme: "dark", Preset: "harbor", Accent: "teal", Density: "compact"}

	if rec := do(t, mux, http.MethodGet, "/api/preferences/appearance", nil, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET without session: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPut, "/api/preferences/appearance", body, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("PUT without session: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPut, "/api/preferences/appearance", body, cookie, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("PUT without CSRF: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPut, "/api/preferences/appearance", Appearance{Theme: "neon"}, cookie, csrf); rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid value: %d", rec.Code)
	}
	if rec := do(t, mux, http.MethodPut, "/api/preferences/appearance", body, cookie, csrf); rec.Code != http.StatusOK {
		t.Fatalf("PUT valid: %d %s", rec.Code, rec.Body.String())
	}

	rec := do(t, mux, http.MethodGet, "/api/preferences/appearance", nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d", rec.Code)
	}
	var got Appearance
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got != body {
		t.Fatalf("GET = %+v, want %+v", got, body)
	}
	_ = store
}
