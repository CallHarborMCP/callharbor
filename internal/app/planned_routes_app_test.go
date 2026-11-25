package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/api"
)

// TestPlannedRESTRoutesNeverSucceedInTheRealApp probes every planned REST
// operation through the running application with a signed-in session. The
// list comes from api.PlannedRoutes, so a new planned route is covered without
// editing this test. No planned route may return 2xx, and none may return a
// JSON success body. A planned GET must reach the not_found envelope and not
// the web UI.
func TestPlannedRESTRoutesNeverSucceedInTheRealApp(t *testing.T) {
	e := startE2E(t, t.TempDir())
	alice := createUser(t, e.admin, "alice")
	if len(api.PlannedRoutes) == 0 {
		t.Fatal("no planned routes to probe")
	}
	for _, r := range api.PlannedRoutes {
		path := strings.ReplaceAll(r.Path, "{call_id}", "planned-probe")
		status, hdr, body := send(t, e.client, r.Method, e.base+path, []byte("{}"), alice.headers(true))
		if status >= 200 && status < 300 {
			t.Fatalf("planned %s %s = %d %s, want a non-2xx", r.Method, path, status, body)
		}
		if strings.Contains(hdr.Get("Content-Type"), "text/html") {
			t.Fatalf("planned %s %s was served by the web UI", r.Method, path)
		}
		if r.Method == http.MethodGet {
			if status != http.StatusNotFound {
				t.Fatalf("planned GET %s = %d %s, want 404", path, status, body)
			}
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &env); err != nil || env.Error.Code != "not_found" {
				t.Fatalf("planned GET %s body = %s, want not_found envelope", path, body)
			}
		}
	}
}

// TestPlannedRESTGetRequiresSessionInTheRealApp checks that a planned GET with
// no session gets the same 401 as every other /api/v1 path.
func TestPlannedRESTGetRequiresSessionInTheRealApp(t *testing.T) {
	e := startE2E(t, t.TempDir())
	for _, r := range api.PlannedRoutes {
		if r.Method != http.MethodGet {
			continue
		}
		path := strings.ReplaceAll(r.Path, "{call_id}", "planned-probe")
		status, _, body := send(t, e.client, http.MethodGet, e.base+path, nil, http.Header{})
		if status != http.StatusUnauthorized {
			t.Fatalf("anonymous planned GET %s = %d %s, want 401", path, status, body)
		}
	}
}
