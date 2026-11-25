package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// compileComponent compiles one component schema from api/openapi.json with a
// JSON Schema 2020-12 validator. The whole document is the resource, so
// internal $ref values such as #/components/schemas/Timestamp resolve.
func compileComponent(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	_, raw := loadOpenAPI(t)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode openapi: %v", err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("file:///openapi.json", doc); err != nil {
		t.Fatalf("add resource: %v", err)
	}
	s, err := c.Compile("file:///openapi.json#/components/schemas/" + name)
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return s
}

// validateBody decodes the exact response bytes and checks them against schema.
func validateBody(t *testing.T, schema *jsonschema.Schema, rec *httptest.ResponseRecorder) {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, rec.Body.String())
	}
	if err := schema.Validate(v); err != nil {
		t.Fatalf("response violates contract: %v\nbody: %s", err, rec.Body.String())
	}
}

// TestREST200BodiesMatchOpenAPIComponents validates the real 200 responses from
// the mounted handlers against the documented component schemas.
func TestREST200BodiesMatchOpenAPIComponents(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name   string
		path   string
		schema string
	}{
		{"profiles", "/api/v1/profiles", "ProfilesResponse"},
		{"calls page", "/api/v1/calls?limit=1", "CallsPage"},
		{"calls page empty", "/api/v1/calls?status=ended", "CallsPage"},
		{"call", "/api/v1/calls/alice-call", "Call"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, f.mux, tc.path, f.aliceCk)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d %s", tc.path, rec.Code, rec.Body.String())
			}
			validateBody(t, compileComponent(t, tc.schema), rec)
		})
	}
}

// TestRESTErrorBodiesMatchOpenAPIError validates real error responses against
// the documented Error component, and checks each code is in the enum.
func TestRESTErrorBodiesMatchOpenAPIError(t *testing.T) {
	f := newFixture(t)
	errSchema := compileComponent(t, "Error")
	cases := []struct {
		name string
		path string
		ck   *http.Cookie
		code int
	}{
		{"unauthenticated", "/api/v1/calls", nil, http.StatusUnauthorized},
		{"not found", "/api/v1/calls/missing", f.aliceCk, http.StatusNotFound},
		{"foreign not found", "/api/v1/calls/alice-call", f.bobCk, http.StatusNotFound},
		{"invalid input", "/api/v1/calls?status=dialing", f.aliceCk, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, f.mux, tc.path, tc.ck)
			if rec.Code != tc.code {
				t.Fatalf("%s = %d %s, want %d", tc.path, rec.Code, rec.Body.String(), tc.code)
			}
			validateBody(t, errSchema, rec)
		})
	}
}

// TestRESTErrorEnvelopeUsesDocumentedCodesOnly keeps the envelope free of
// extra fields. The OpenAPI Error schema forbids additionalProperties, so the
// validation above already covers this; this test names the rule explicitly.
func TestRESTErrorEnvelopeUsesDocumentedCodesOnly(t *testing.T) {
	f := newFixture(t)
	rec := get(t, f.mux, "/api/v1/calls/missing", f.aliceCk)
	var env map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || len(env["error"]) != 2 || strings.Contains(rec.Body.String(), "Cause") {
		t.Fatalf("envelope has extra fields: %s", rec.Body.String())
	}
}
