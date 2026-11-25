package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTelephonyBodiesMatchOpenAPIComponents validates real telephony responses
// from the mounted handlers against the documented component schemas. It covers
// the profile, the list, the validation result, the grant list and the error
// envelopes the telephony routes can return.
func TestTelephonyBodiesMatchOpenAPIComponents(t *testing.T) {
	f := newTelFixture(t)
	// Grants are admin-only and valid only on a profile the admin owns, so the
	// profile the grant list reads is created by the admin.
	id := createTel(t, f, "admin", validWrite())

	profile := f.do(t, "admin", http.MethodGet, "/api/v1/telephony-profiles/"+id, nil, false)
	if profile.Code != http.StatusOK {
		t.Fatalf("get = %d %s", profile.Code, profile.Body.String())
	}
	validateBody(t, compileComponent(t, "TelephonyProfile"), profile)

	list := f.do(t, "admin", http.MethodGet, "/api/v1/telephony-profiles", nil, false)
	if list.Code != http.StatusOK {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}
	validateBody(t, compileComponent(t, "TelephonyProfileList"), list)

	validate := f.do(t, "admin", http.MethodPost, "/api/v1/telephony-profiles/"+id+"/validate", nil, true)
	if validate.Code != http.StatusOK {
		t.Fatalf("validate = %d %s", validate.Code, validate.Body.String())
	}
	validateBody(t, compileComponent(t, "TelephonyValidation"), validate)

	if rec := f.do(t, "admin", http.MethodPut, "/api/v1/telephony-profiles/"+id+"/grants/bob", nil, true); rec.Code != http.StatusNoContent {
		t.Fatalf("grant = %d %s", rec.Code, rec.Body.String())
	}
	grants := f.do(t, "admin", http.MethodGet, "/api/v1/telephony-profiles/"+id+"/grants", nil, false)
	if grants.Code != http.StatusOK {
		t.Fatalf("grants = %d %s", grants.Code, grants.Body.String())
	}
	validateBody(t, compileComponent(t, "TelephonyGrantList"), grants)
}

// TestTelephonyErrorBodiesMatchOpenAPIError validates real telephony error
// envelopes against the documented Error component, including a 409 and the
// unkeyed 503.
func TestTelephonyErrorBodiesMatchOpenAPIError(t *testing.T) {
	errSchema := compileComponent(t, "Error")
	f := newTelFixture(t)
	id := createTel(t, f, "alice", validWrite())
	cases := []struct {
		name string
		rec  func() *httptest.ResponseRecorder
		code int
	}{
		{"unauthenticated", func() *httptest.ResponseRecorder {
			return f.do(t, "", http.MethodGet, "/api/v1/telephony-profiles", nil, false)
		}, http.StatusUnauthorized},
		{"malformed id", func() *httptest.ResponseRecorder {
			return f.do(t, "alice", http.MethodGet, "/api/v1/telephony-profiles/bad%20id", nil, false)
		}, http.StatusNotFound},
		{"csrf missing", func() *httptest.ResponseRecorder {
			return f.do(t, "alice", http.MethodPost, "/api/v1/telephony-profiles/"+id+"/validate", nil, false)
		}, http.StatusForbidden},
		{"duplicate name", func() *httptest.ResponseRecorder {
			return f.do(t, "alice", http.MethodPost, "/api/v1/telephony-profiles", validWrite(), true)
		}, http.StatusConflict},
		{"invalid body", func() *httptest.ResponseRecorder {
			return f.do(t, "alice", http.MethodPost, "/api/v1/telephony-profiles", map[string]any{"name": ""}, true)
		}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.rec()
			if rec.Code != tc.code {
				t.Fatalf("%s = %d %s, want %d", tc.name, rec.Code, rec.Body.String(), tc.code)
			}
			validateBody(t, errSchema, rec)
		})
	}

	unkeyed := newTelFixtureUnkeyed(t)
	rec := unkeyed.do(t, "alice", http.MethodPost, "/api/v1/telephony-profiles", validWrite(), true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unkeyed create = %d %s, want 503", rec.Code, rec.Body.String())
	}
	validateBody(t, errSchema, rec)
}
