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

// TestNoOpenAPIOperationIsPlanned asserts the current contract: every documented
// REST operation is implemented, so there is nothing to probe as planned. If an
// operation is documented as planned later, the mechanism tests below still
// cover its path, and this test must be updated with it.
func TestNoOpenAPIOperationIsPlanned(t *testing.T) {
	d, _ := loadOpenAPI(t)
	for path, ops := range d.Paths {
		for method, raw := range ops {
			op, ok := raw.(map[string]any)
			if ok && op["x-callharbor-availability"] == "planned" {
				t.Fatalf("%s %s is documented as planned; update PlannedRoutes and this test", strings.ToUpper(method), path)
			}
		}
	}
}

// TestPlannedRouteIsNotMounted exercises the planned-route mechanism with a
// synthetic planned GET. The mux must answer it through the planned handler: a
// signed-in request gets 404 not_found in the JSON envelope, and an anonymous
// one gets 401 before any answer. A wrong method on that path is 405 from the
// mux, and the path has no handler behind it.
func TestPlannedRouteIsNotMounted(t *testing.T) {
	planned := Route{Method: http.MethodGet, Path: "/api/v1/planned-probe/{call_id}"}
	f := newFixtureWithPlanned(t, planned)
	probe := plannedProbe(planned.Path)

	req := httptest.NewRequest(http.MethodGet, probe, nil)
	req.AddCookie(f.aliceCk)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || errCode(t, rec) != "not_found" {
		t.Fatalf("planned GET = %d %s, want 404 not_found", rec.Code, rec.Body.String())
	}
	validateBody(t, compileComponent(t, "Error"), rec)

	anon := httptest.NewRequest(http.MethodGet, probe, nil)
	anonRec := httptest.NewRecorder()
	f.mux.ServeHTTP(anonRec, anon)
	if anonRec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous planned GET = %d %s, want 401", anonRec.Code, anonRec.Body.String())
	}

	post := httptest.NewRequest(http.MethodPost, probe, strings.NewReader("{}"))
	post.AddCookie(f.aliceCk)
	postRec := httptest.NewRecorder()
	f.mux.ServeHTTP(postRec, post)
	if postRec.Code != http.StatusMethodNotAllowed && postRec.Code != http.StatusNotFound {
		t.Fatalf("planned POST = %d %s, want 404 or 405", postRec.Code, postRec.Body.String())
	}
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

// TestEveryAvailableRouteIsMounted probes each entry in Routes and checks it
// is reached, not answered by the mux's own 404. It does not check that
// nothing else is mounted; the planned-route tests cover unmounted paths.
func TestEveryAvailableRouteIsMounted(t *testing.T) {
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
