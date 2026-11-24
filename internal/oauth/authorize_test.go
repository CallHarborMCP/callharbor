package oauth

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// location returns the redirect target of resp, failing if it is not a
// redirect.
func location(t *testing.T, resp *http.Response) *url.URL {
	t.Helper()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("bad Location: %v", err)
	}
	return u
}

func get(t *testing.T, target string) *http.Response {
	t.Helper()
	resp, err := noRedirectClient().Get(target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestAuthorizeRedirectsToConsentWithRequestID(t *testing.T) {
	f := newFixture(t)
	resp := get(t, f.authorizeURL(nil))
	loc := location(t, resp)
	if loc.Path != "/oauth/consent" {
		t.Fatalf("redirect path = %q, want /oauth/consent", loc.Path)
	}
	id := loc.Query().Get("request")
	if !strings.HasPrefix(id, prefixRequest) {
		t.Fatalf("request id = %q, want %s prefix", id, prefixRequest)
	}
	req, err := f.store.GetOAuthAuthRequest(f.ctx, id)
	if err != nil {
		t.Fatalf("auth request not stored: %v", err)
	}
	if req.Scopes != "mcp:read mcp:call" || req.Resource != f.cfg.Resource() {
		t.Fatalf("stored scopes/resource = %q / %q", req.Scopes, req.Resource)
	}
	if req.CodeChallenge != testChallenge || req.State != "xyz" {
		t.Fatalf("stored challenge/state = %q / %q", req.CodeChallenge, req.State)
	}
}

func TestAuthorizeRequestExpiresAfterTenMinutes(t *testing.T) {
	f := newFixture(t)
	id := location(t, get(t, f.authorizeURL(nil))).Query().Get("request")
	req, err := f.store.GetOAuthAuthRequest(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if req.ExpiresAt != formatTime(f.clock.Now().Add(authRequestTTL)) {
		t.Fatalf("expires_at = %q, want now+10m", req.ExpiresAt)
	}
}

func TestAuthorizeRejectsUnknownClientWithoutRedirect(t *testing.T) {
	f := newFixture(t)
	resp := get(t, f.authorizeURL(map[string]string{"client_id": "https://evil.example/m.json"}))
	if resp.StatusCode == http.StatusFound {
		t.Fatalf("unknown client was redirected: %s", resp.Header.Get("Location"))
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}
}

func TestAuthorizeRejectsUnregisteredRedirectWithoutRedirect(t *testing.T) {
	f := newFixture(t)
	resp := get(t, f.authorizeURL(map[string]string{"redirect_uri": "https://attacker.example/steal"}))
	if resp.StatusCode == http.StatusFound {
		t.Fatalf("unregistered redirect_uri was redirected to: %s", resp.Header.Get("Location"))
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAuthorizeRejectsMissingRedirectWithoutRedirect(t *testing.T) {
	f := newFixture(t)
	resp := get(t, f.authorizeURL(map[string]string{"redirect_uri": ""}))
	if resp.StatusCode == http.StatusFound {
		t.Fatal("missing redirect_uri was redirected")
	}
}

func TestAuthorizeRejectsFragmentRedirectWithoutRedirect(t *testing.T) {
	f := newFixture(t)
	f.registerClient("https://frag.example/m.json", []string{"https://frag.example/cb#x"})
	resp := get(t, f.authorizeURL(map[string]string{
		"client_id":    "https://frag.example/m.json",
		"redirect_uri": "https://frag.example/cb#x",
	}))
	if resp.StatusCode == http.StatusFound {
		t.Fatal("fragment redirect_uri was redirected")
	}
}

func TestAuthorizeRejectsPlainHTTPRedirectForNonLoopback(t *testing.T) {
	f := newFixture(t)
	f.registerClient("https://plain.example/m.json", []string{"http://plain.example/cb"})
	resp := get(t, f.authorizeURL(map[string]string{
		"client_id":    "https://plain.example/m.json",
		"redirect_uri": "http://plain.example/cb",
	}))
	if resp.StatusCode == http.StatusFound {
		t.Fatal("non-loopback http redirect_uri was redirected")
	}
}

func TestAuthorizeAllowsLoopbackHTTPRedirect(t *testing.T) {
	f := newFixture(t)
	f.registerClient("https://loop.example/m.json", []string{"http://127.0.0.1:8765/cb"})
	resp := get(t, f.authorizeURL(map[string]string{
		"client_id":    "https://loop.example/m.json",
		"redirect_uri": "http://127.0.0.1:8765/cb",
	}))
	location(t, resp)
}

func TestAuthorizeErrorRedirectCarriesStateAndIss(t *testing.T) {
	f := newFixture(t)
	resp := get(t, f.authorizeURL(map[string]string{"scope": "mcp:read openid"}))
	loc := location(t, resp)
	if loc.Host != "client.example" {
		t.Fatalf("error redirect host = %q, want the registered client", loc.Host)
	}
	q := loc.Query()
	if q.Get("error") != "invalid_scope" {
		t.Fatalf("error = %q, want invalid_scope", q.Get("error"))
	}
	if q.Get("state") != "xyz" {
		t.Fatalf("state = %q, want xyz", q.Get("state"))
	}
	if q.Get("iss") != f.cfg.Issuer() {
		t.Fatalf("iss = %q, want %q (RFC 9207)", q.Get("iss"), f.cfg.Issuer())
	}
}

func TestAuthorizeRejectsUnknownScopeInvalidScope(t *testing.T) {
	f := newFixture(t)
	loc := location(t, get(t, f.authorizeURL(map[string]string{"scope": "mcp:read admin:everything"})))
	if loc.Query().Get("error") != "invalid_scope" {
		t.Fatalf("error = %q, want invalid_scope", loc.Query().Get("error"))
	}
}

func TestAuthorizeScopeDefaultsToAllMCPScopesWhenOmitted(t *testing.T) {
	f := newFixture(t)
	id := location(t, get(t, f.authorizeURL(map[string]string{"scope": ""}))).Query().Get("request")
	req, err := f.store.GetOAuthAuthRequest(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if req.Scopes != "mcp:read mcp:call" {
		t.Fatalf("scopes = %q, want both mcp scopes", req.Scopes)
	}
}

func TestAuthorizeRequiresPKCEChallenge(t *testing.T) {
	f := newFixture(t)
	loc := location(t, get(t, f.authorizeURL(map[string]string{"code_challenge": "", "code_challenge_method": ""})))
	if loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("error = %q, want invalid_request", loc.Query().Get("error"))
	}
}

func TestAuthorizeRejectsPlainPKCEMethod(t *testing.T) {
	f := newFixture(t)
	loc := location(t, get(t, f.authorizeURL(map[string]string{"code_challenge_method": "plain"})))
	if loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("error = %q, want invalid_request", loc.Query().Get("error"))
	}
	if loc.Query().Get("iss") != f.cfg.Issuer() {
		t.Fatal("plain PKCE error redirect missing iss")
	}
}

func TestAuthorizeRejectsUnknownPKCEMethodWithoutDefaulting(t *testing.T) {
	f := newFixture(t)
	loc := location(t, get(t, f.authorizeURL(map[string]string{"code_challenge_method": "S512"})))
	if loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("error = %q, want invalid_request", loc.Query().Get("error"))
	}
}

func TestAuthorizeRequiresResource(t *testing.T) {
	f := newFixture(t)
	loc := location(t, get(t, f.authorizeURL(map[string]string{"resource": ""})))
	if loc.Query().Get("error") != "invalid_target" {
		t.Fatalf("error = %q, want invalid_target", loc.Query().Get("error"))
	}
}

func TestAuthorizeRejectsWrongResource(t *testing.T) {
	f := newFixture(t)
	loc := location(t, get(t, f.authorizeURL(map[string]string{"resource": "https://other.example/mcp"})))
	if loc.Query().Get("error") != "invalid_target" {
		t.Fatalf("error = %q, want invalid_target", loc.Query().Get("error"))
	}
}

func TestAuthorizeRejectsDuplicateResource(t *testing.T) {
	f := newFixture(t)
	target := f.authorizeURL(nil) + "&resource=" + url.QueryEscape(f.cfg.Resource())
	loc := location(t, get(t, target))
	if loc.Query().Get("error") != "invalid_target" {
		t.Fatalf("error = %q, want invalid_target for two resources", loc.Query().Get("error"))
	}
}

func TestAuthorizeRejectsNonCodeResponseType(t *testing.T) {
	f := newFixture(t)
	loc := location(t, get(t, f.authorizeURL(map[string]string{"response_type": "token"})))
	if loc.Query().Get("error") == "" {
		t.Fatal("token response_type was accepted")
	}
	if loc.Query().Get("iss") != f.cfg.Issuer() {
		t.Fatal("response_type error redirect missing iss")
	}
}

func TestAuthorizeRejectsRequestObject(t *testing.T) {
	f := newFixture(t)
	loc := location(t, get(t, f.authorizeURL(map[string]string{"request": "eyJhbGciOiJub25lIn0.e30."})))
	if loc.Query().Get("error") != "request_not_supported" {
		t.Fatalf("error = %q, want request_not_supported", loc.Query().Get("error"))
	}
}
