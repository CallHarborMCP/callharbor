package api

import (
	"encoding/json"
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

// TestOpenAPIPathsMatchImplementedRoutes checks the document against the code
// in both directions. Operations without x-callharbor-availability are
// available and must equal Routes. Operations marked planned must equal
// PlannedRoutes. Nothing is documented that the code does not know about.
func TestOpenAPIPathsMatchImplementedRoutes(t *testing.T) {
	d, _ := loadOpenAPI(t)
	available := map[string]bool{}
	planned := map[string]bool{}
	for path, ops := range d.Paths {
		for method, raw := range ops {
			op, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("%s %s is not an operation object", method, path)
			}
			key := strings.ToUpper(method) + " " + path
			switch op["x-callharbor-availability"] {
			case nil:
				available[key] = true
			case "planned":
				planned[key] = true
			default:
				t.Fatalf("%s has unknown x-callharbor-availability %v", key, op["x-callharbor-availability"])
			}
		}
	}
	mounted := map[string]bool{}
	for _, r := range Routes {
		mounted[r.Method+" "+r.Path] = true
	}
	plannedCode := map[string]bool{}
	for _, r := range PlannedRoutes {
		plannedCode[r.Method+" "+r.Path] = true
	}
	if strings.Join(sortedKeys(available), ",") != strings.Join(sortedKeys(mounted), ",") {
		t.Fatalf("available operations = %v\nRoutes = %v", sortedKeys(available), sortedKeys(mounted))
	}
	if strings.Join(sortedKeys(planned), ",") != strings.Join(sortedKeys(plannedCode), ",") {
		t.Fatalf("planned operations = %v\nPlannedRoutes = %v", sortedKeys(planned), sortedKeys(plannedCode))
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
	want := service.CallStates()
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("documented statuses = %v", got)
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
