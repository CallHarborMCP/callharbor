package oauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
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

// authorizeFrom runs a valid authorization request from remoteAddr through the
// handler directly, so the per-IP key can vary.
func authorizeFrom(t *testing.T, f *fixture, remoteAddr string) *http.Response {
	t.Helper()
	target, err := url.Parse(f.authorizeURL(nil))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+target.RawQuery, nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	f.srv.handleAuthorize(rec, req)
	return rec.Result()
}

// TestAuthorizeDropsExpiredRequestOnNextRequest: an abandoned request that has
// expired is removed by the next authorization, so it cannot accumulate.
func TestAuthorizeDropsExpiredRequestOnNextRequest(t *testing.T) {
	f := newFixture(t)
	abandoned := location(t, get(t, f.authorizeURL(nil))).Query().Get("request")
	f.clock.Advance(authRequestTTL + time.Second)
	if _, err := f.store.GetOAuthAuthRequest(f.ctx, abandoned); err != nil {
		t.Fatalf("abandoned request missing before purge: %v", err)
	}
	location(t, get(t, f.authorizeURL(nil)))
	if _, err := f.store.GetOAuthAuthRequest(f.ctx, abandoned); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expired request survived the next authorize: err = %v", err)
	}
}

// TestAuthorizeRateLimitReturns429WithoutRedirect: past the per-IP budget an
// authorization request gets a 429 page and no redirect, and other IPs and
// later windows are not affected.
func TestAuthorizeRateLimitReturns429WithoutRedirect(t *testing.T) {
	f := newFixture(t)
	const ip = "198.51.100.40:4000"
	for i := 0; i < authorizePerWindow; i++ {
		resp := authorizeFrom(t, f, ip)
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("request %d status = %d, want 302", i+1, resp.StatusCode)
		}
	}
	resp := authorizeFrom(t, f, ip)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-limit status = %d, want 429", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("429 must not redirect, got Location %q", loc)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	if other := authorizeFrom(t, f, "198.51.100.41:4000"); other.StatusCode != http.StatusFound {
		t.Fatalf("other IP status = %d, want 302", other.StatusCode)
	}
	f.clock.Advance(authorizeWindow + time.Second)
	if after := authorizeFrom(t, f, ip); after.StatusCode != http.StatusFound {
		t.Fatalf("after window status = %d, want 302", after.StatusCode)
	}
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

// TestAuthorizeLoopbackMatchIgnoresOnlyThePort: a native app may use any port on
// loopback, so a port change is accepted. A change of host, scheme or path is
// not, even though the library's loopback check would accept it.
func TestAuthorizeLoopbackMatchIgnoresOnlyThePort(t *testing.T) {
	const cid = "https://loop.example/m.json"
	f := newFixture(t)
	f.registerClient(cid, []string{"http://127.0.0.1:8765/cb"})

	t.Run("port change accepted", func(t *testing.T) {
		location(t, get(t, f.authorizeURL(map[string]string{
			"client_id": cid, "redirect_uri": "http://127.0.0.1:55001/cb",
		})))
	})
	for name, uri := range map[string]string{
		"host swap":   "http://localhost:8765/cb",
		"other host":  "http://[::1]:8765/cb",
		"scheme swap": "https://127.0.0.1:8765/cb",
		"path change": "http://127.0.0.1:8765/other",
	} {
		t.Run(name, func(t *testing.T) {
			resp := get(t, f.authorizeURL(map[string]string{"client_id": cid, "redirect_uri": uri}))
			if resp.StatusCode == http.StatusFound {
				t.Fatalf("redirect_uri %q was redirected, want a refusal without redirect", uri)
			}
		})
	}
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

// TestAuthorizeMalformedChallengeIsInvalidRequestViaRedirect: an S256 challenge
// is exactly 43 base64url characters (RFC 7636 section 4.2). A malformed one is
// refused as invalid_request through the validated redirect, not as a database
// CHECK failure that reaches the client as server_error.
func TestAuthorizeMalformedChallengeIsInvalidRequestViaRedirect(t *testing.T) {
	f := newFixture(t)
	for name, challenge := range map[string]string{
		"too short":      strings.Repeat("A", 42),
		"too long":       strings.Repeat("A", 129),
		"padding":        strings.Repeat("A", 42) + "=",
		"non base64url":  strings.Repeat("A", 42) + "+",
		"contains space": strings.Repeat("A", 20) + " " + strings.Repeat("A", 22),
	} {
		t.Run(name, func(t *testing.T) {
			loc := location(t, get(t, f.authorizeURL(map[string]string{"code_challenge": challenge})))
			if loc.Query().Get("error") != "invalid_request" {
				t.Fatalf("error = %q, want invalid_request", loc.Query().Get("error"))
			}
			if loc.Query().Get("state") != "xyz" || loc.Query().Get("iss") != f.cfg.Issuer() {
				t.Fatalf("error redirect missing state or iss: %q", loc.RawQuery)
			}
		})
	}
}

// TestAuthorizeOverlongStateIsInvalidRequestViaRedirect: state is capped at
// 1024 bytes, the same bound the schema enforces. A longer state is refused as
// invalid_request through the validated redirect instead of a database failure.
func TestAuthorizeOverlongStateIsInvalidRequestViaRedirect(t *testing.T) {
	f := newFixture(t)
	loc := location(t, get(t, f.authorizeURL(map[string]string{"state": strings.Repeat("s", 1025)})))
	if loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("error = %q, want invalid_request", loc.Query().Get("error"))
	}
	if loc.Query().Get("iss") != f.cfg.Issuer() {
		t.Fatal("overlong state error redirect missing iss")
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
