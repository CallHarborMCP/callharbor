package app

import (
	"net/http"
	"strings"
	"testing"
)

// TestMCPNonPOSTMethodsAnswer405InTheRealApp sends each non-POST method to
// /mcp through the running application. The Streamable HTTP spec requires a
// server to answer GET with 405 when it offers no SSE stream, and the server
// has none. Without an explicit route, GET and HEAD would fall through to the
// SPA catch-all "GET /" and return 404 or 503. The 405 must carry an Allow
// header and must not depend on whether the web UI is built.
func TestMCPNonPOSTMethodsAnswer405InTheRealApp(t *testing.T) {
	e := startE2E(t, t.TempDir())
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodPut} {
		status, hdr, _ := send(t, e.client, method, e.resource(), nil, http.Header{})
		if status != http.StatusMethodNotAllowed {
			t.Fatalf("%s /mcp = %d, want 405", method, status)
		}
		allow := hdr.Get("Allow")
		if allow != "POST, OPTIONS" {
			t.Fatalf("%s /mcp Allow = %q, want %q", method, allow, "POST, OPTIONS")
		}
	}
}

// TestMCPNonPOSTMethodsDoNotRequireAToken confirms the 405 answers before
// authentication. A client that sends no token must still learn the method is
// not allowed, not receive a 401 challenge.
func TestMCPNonPOSTMethodsDoNotRequireAToken(t *testing.T) {
	e := startE2E(t, t.TempDir())
	status, hdr, _ := send(t, e.client, http.MethodGet, e.resource(), nil, http.Header{})
	if status == http.StatusUnauthorized || hdr.Get("WWW-Authenticate") != "" {
		t.Fatalf("GET /mcp without token = %d %q, want 405 with no challenge", status, hdr.Get("WWW-Authenticate"))
	}
}

// TestMCPNonPOSTMethodsCarryCORSPolicy checks that the 405 has the same CORS
// header policy as POST, so a browser can read the Allow header on failure.
func TestMCPNonPOSTMethodsCarryCORSPolicy(t *testing.T) {
	e := startE2E(t, t.TempDir())
	_, hdr, _ := send(t, e.client, http.MethodGet, e.resource(), nil, http.Header{})
	if got := hdr.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("405 Allow-Origin = %q, want *", got)
	}
	if strings.Contains(hdr.Get("Access-Control-Allow-Credentials"), "true") {
		t.Fatal("405 allows credentials")
	}
}
