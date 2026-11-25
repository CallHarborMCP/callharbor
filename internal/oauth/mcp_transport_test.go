package oauth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mcpProbe is a stand-in transport. It records each request it receives and
// answers 200, so a test can tell a request that reached the transport from one
// that was refused earlier.
type mcpProbe struct {
	calls int
	body  string
	user  string
}

func (p *mcpProbe) wrap(bearer func(http.Handler) http.Handler, _ string) http.Handler {
	return bearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.calls++
		if pr, ok := PrincipalFromContext(r.Context()); ok {
			p.user = pr.UserID
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			// The body limit surfaces here. A truncated body is never recorded
			// as processed.
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		p.body = string(b)
		w.WriteHeader(http.StatusOK)
	}))
}

// mcpMux mounts the real /mcp routes around a probe transport.
func (f *fixture) mcpMux(p *mcpProbe) *http.ServeMux {
	mux := http.NewServeMux()
	f.srv.RegisterMCP(mux, p.wrap)
	return mux
}

// TestMCPPreflightNeedsNoAuthAndNeverCarriesCredentials checks CORS on /mcp:
// the preflight answers 204 without a token, the wildcard origin is set, and
// credentials are never allowed.
func TestMCPPreflightNeedsNoAuthAndNeverCarriesCredentials(t *testing.T) {
	f := newFixture(t)
	p := &mcpProbe{}
	mux := f.mcpMux(p)

	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	req.Header.Set("Origin", "https://client.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization, content-type, mcp-protocol-version")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin = %q, want *", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("preflight sends Allow-Credentials = %q", got)
	}
	if methods := rec.Header().Get("Access-Control-Allow-Methods"); methods != "POST, OPTIONS" {
		t.Fatalf("Allow-Methods = %q, want POST, OPTIONS", methods)
	}
	if p.calls != 0 {
		t.Fatal("preflight reached the transport")
	}
}

// TestMCPNonPOSTMethodsAreRejected checks that only POST reaches the transport.
// GET and DELETE are not routed, so the mux answers 405 before auth runs.
func TestMCPNonPOSTMethodsAreRejected(t *testing.T) {
	f := newFixture(t)
	p := &mcpProbe{}
	mux := f.mcpMux(p)
	access, _ := f.bearerFixture(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		req := httptest.NewRequest(method, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+access)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /mcp = %d, want 405", method, rec.Code)
		}
	}
	if p.calls != 0 {
		t.Fatal("a non-POST method reached the transport")
	}
}

// TestMCPExposesOnlyChallengeHeader checks that browsers can read only the
// WWW-Authenticate header from a /mcp response.
func TestMCPExposesOnlyChallengeHeader(t *testing.T) {
	f := newFixture(t)
	mux := f.mcpMux(&mcpProbe{})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "WWW-Authenticate" {
		t.Fatalf("Expose-Headers = %q, want WWW-Authenticate", got)
	}
}

// TestMCPOversizedPOSTIsNeverProcessed sends a body over the single limit with
// a valid token. The request must fail with 413 and the transport must not
// have processed the body.
func TestMCPOversizedPOSTIsNeverProcessed(t *testing.T) {
	f := newFixture(t)
	p := &mcpProbe{}
	mux := f.mcpMux(p)
	access, _ := f.bearerFixture(t)
	big := strings.Repeat("a", MCPBodyLimit+1)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer "+access)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized POST = %d, want 413", rec.Code)
	}
	if p.body != "" {
		t.Fatalf("oversized body was processed: %d bytes", len(p.body))
	}
}

// TestMCPBodyAtLimitIsAccepted confirms the limit is exact: a body of exactly
// MCPBodyLimit bytes is read in full.
func TestMCPBodyAtLimitIsAccepted(t *testing.T) {
	f := newFixture(t)
	p := &mcpProbe{}
	mux := f.mcpMux(p)
	access, _ := f.bearerFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(strings.Repeat("a", MCPBodyLimit)))
	req.Header.Set("Authorization", "Bearer "+access)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || len(p.body) != MCPBodyLimit {
		t.Fatalf("body at limit = %d, read %d bytes", rec.Code, len(p.body))
	}
}

// TestMCPCallOnlyTokenIsRefusedBeforeTransport checks that a valid token
// without mcp:read is refused before the transport runs.
func TestMCPCallOnlyTokenIsRefusedBeforeTransport(t *testing.T) {
	f := newFixture(t)
	p := &mcpProbe{}
	mux := f.mcpMux(p)
	access := f.callOnlyToken(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+access)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="insufficient_scope"`) {
		t.Fatalf("no mcp:read = %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	if p.calls != 0 {
		t.Fatal("a token without mcp:read reached the transport")
	}
}

// TestMCPPrincipalFromBearerReachesTransport confirms that a valid token
// reaches the transport with the principal attached to the request context.
func TestMCPPrincipalFromBearerReachesTransport(t *testing.T) {
	f := newFixture(t)
	p := &mcpProbe{}
	mux := f.mcpMux(p)
	access, _ := f.bearerFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+access)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || p.user != f.userID {
		t.Fatalf("valid bearer = %d user=%q, want 200 and %q", rec.Code, p.user, f.userID)
	}
}
