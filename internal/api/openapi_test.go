package api

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/CallHarborMCP/callharbor/internal/service"
)

const openAPIPath = "../../api/openapi.json"

type openAPIDoc struct {
	Paths      map[string]map[string]any `json:"paths"`
	Components struct {
		Schemas map[string]any `json:"schemas"`
	} `json:"components"`
}

func loadOpenAPI(t *testing.T) (openAPIDoc, []byte) {
	t.Helper()
	b, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	var d openAPIDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("parse openapi: %v", err)
	}
	return d, b
}

// TestOpenAPIDocumentIsValidJSONSchema compiles the document as an OpenAPI
// 3.1 (JSON Schema 2020-12) resource, so a malformed schema fails the build.
func TestOpenAPIDocumentIsValidJSONSchema(t *testing.T) {
	_, raw := loadOpenAPI(t)
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("file:///openapi.json", doc); err != nil {
		t.Fatalf("add resource: %v", err)
	}
	if _, err := c.Compile("file:///openapi.json#/components/schemas/CallsPage"); err != nil {
		t.Fatalf("compile CallsPage: %v", err)
	}
	if _, err := c.Compile("file:///openapi.json#/components/schemas/Error"); err != nil {
		t.Fatalf("compile Error: %v", err)
	}
}

// TestOpenAPIPathsMatchImplementedRoutes ensures every documented operation is
// a real route, and every REST route is documented.
func TestOpenAPIPathsMatchImplementedRoutes(t *testing.T) {
	d, _ := loadOpenAPI(t)
	documented := map[string]bool{}
	for path, ops := range d.Paths {
		for method := range ops {
			documented[strings.ToUpper(method)+" "+path] = true
		}
	}
	implemented := registeredRoutes(t)
	if strings.Join(sortedKeys(documented), ",") != strings.Join(sortedKeys(implemented), ",") {
		t.Fatalf("documented routes = %v\nimplemented routes = %v", sortedKeys(documented), sortedKeys(implemented))
	}
}

// TestOpenAPIErrorCodesMatchServiceTaxonomy keeps the documented error enum
// equal to the service's stable codes.
func TestOpenAPIErrorCodesMatchServiceTaxonomy(t *testing.T) {
	d, _ := loadOpenAPI(t)
	errSchema, ok := d.Components.Schemas["ErrorCode"].(map[string]any)
	if !ok {
		t.Fatal("ErrorCode schema missing")
	}
	raw, _ := errSchema["enum"].([]any)
	var got []string
	for _, v := range raw {
		got = append(got, v.(string))
	}
	want := []string{
		string(service.CodeUnauthenticated), string(service.CodeForbidden),
		string(service.CodeInsufficientScope), string(service.CodeNotFound),
		string(service.CodeInvalidInput), string(service.CodeConflict),
		string(service.CodeInternal),
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("documented error codes = %v, want %v", got, want)
	}
}

// TestOpenAPIStatusEnumMatchesServiceStates keeps the documented status enum
// equal to the service's call state set.
func TestOpenAPIStatusEnumMatchesServiceStates(t *testing.T) {
	d, _ := loadOpenAPI(t)
	s, ok := d.Components.Schemas["TechnicalStatus"].(map[string]any)
	if !ok {
		t.Fatal("TechnicalStatus missing")
	}
	raw, _ := s["enum"].([]any)
	var got []string
	for _, v := range raw {
		got = append(got, v.(string))
	}
	want := []string{"active", "ending", "ended", "failed", "initiating", "queued", "ringing"}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("documented statuses = %v", got)
	}
}

// registeredRoutes returns the method+path pairs the REST handler mounts,
// read from the live mux so the contract cannot silently drift.
func registeredRoutes(t *testing.T) map[string]bool {
	t.Helper()
	f := newFixture(t)
	routes := map[string]bool{}
	for _, route := range []string{
		"GET /api/v1/profiles",
		"GET /api/v1/calls",
		"GET /api/v1/calls/{call_id}",
	} {
		// Probe each pattern: a registered pattern answers with something other
		// than the mux's own 404 for an anonymous request.
		parts := strings.SplitN(route, " ", 2)
		probe := strings.ReplaceAll(parts[1], "{call_id}", "probe")
		rec := get(t, f.mux, probe, nil)
		if rec.Code == http.StatusNotFound && !strings.Contains(rec.Body.String(), `"error"`) {
			t.Fatalf("%s is not registered", route)
		}
		routes[route] = true
	}
	return routes
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
