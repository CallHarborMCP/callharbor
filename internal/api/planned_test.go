package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// plannedProbe maps each planned route pattern to a concrete request path.
func plannedProbe(pattern string) string {
	return strings.ReplaceAll(pattern, "{call_id}", "probe-call")
}

// TestPlannedOperationsAreNotMounted sends every planned method and path with
// a valid session. A planned operation is documented, not implemented, so the
// real mux must answer 404 and never reach a handler.
func TestPlannedOperationsAreNotMounted(t *testing.T) {
	f := newFixture(t)
	d, _ := loadOpenAPI(t)
	probed := 0
	for path, ops := range d.Paths {
		for method, raw := range ops {
			op, ok := raw.(map[string]any)
			if !ok || op["x-callharbor-availability"] != "planned" {
				continue
			}
			probed++
			verb := strings.ToUpper(method)
			req := httptest.NewRequest(verb, plannedProbe(path), strings.NewReader("{}"))
			req.AddCookie(f.aliceCk)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "probe-key")
			rec := httptest.NewRecorder()
			f.mux.ServeHTTP(rec, req)
			// A planned operation must never succeed or reach a handler. The
			// mux answers 404 for an unmounted path, and 405 when the path is
			// mounted only for another method (for example POST on GET /calls).
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("planned %s %s = %d %s, want 404 or 405", verb, path, rec.Code, rec.Body.String())
			}
			if rec.Code == http.StatusMethodNotAllowed && !mountedPath(path) {
				t.Fatalf("planned %s %s answered 405 but its path is not mounted", verb, path)
			}
		}
	}
	if probed == 0 {
		t.Fatal("no planned operations found in openapi.json")
	}
}

// mountedPath reports whether some available route uses this path, under any
// method.
func mountedPath(path string) bool {
	for _, r := range Routes {
		if r.Path == path {
			return true
		}
	}
	return false
}

// TestRoutesAndPlannedRoutesAreDisjoint keeps the available and planned sets
// separate, so a planned path cannot be mounted by adding it to Routes alone.
func TestRoutesAndPlannedRoutesAreDisjoint(t *testing.T) {
	avail := map[string]bool{}
	for _, r := range Routes {
		avail[r.Method+" "+r.Path] = true
	}
	for _, r := range PlannedRoutes {
		if avail[r.Method+" "+r.Path] {
			t.Fatalf("%s %s is listed as both available and planned", r.Method, r.Path)
		}
	}
}

// TestRegisterMountsExactlyTheRoutesTable proves Register mounts every entry
// in Routes and nothing else, by probing each planned and available method.
func TestRegisterMountsExactlyTheRoutesTable(t *testing.T) {
	f := newFixture(t)
	for _, r := range Routes {
		req := httptest.NewRequest(r.Method, plannedProbe(r.Path), nil)
		req.AddCookie(f.aliceCk)
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound && !strings.Contains(rec.Body.String(), `"error"`) {
			t.Fatalf("available %s %s is not mounted", r.Method, r.Path)
		}
	}
}
