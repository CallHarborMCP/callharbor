package oauth

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// register posts a registration body from remoteAddr through the real handler.
// httptest connections all come from 127.0.0.1, so the address is set on the
// request itself, which is what the handler reads.
func (f *fixture) register(t *testing.T, contentType, remoteAddr, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	f.srv.handleRegister(rec, req)
	return rec.Result()
}

// registerOK posts a valid registration and returns the new client_id.
func (f *fixture) registerOK(t *testing.T, body string) string {
	t.Helper()
	resp := f.register(t, "application/json", "203.0.113.10:4000", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want 201", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	id, _ := out["client_id"].(string)
	if id == "" {
		t.Fatalf("no client_id in %v", out)
	}
	return id
}

// registrationBody is a valid RFC 7591 request with optional overrides.
func registrationBody(overrides map[string]any) string {
	doc := map[string]any{
		"client_name":   "Dynamic Test Client",
		"redirect_uris": []string{testRedirectURI},
	}
	for k, v := range overrides {
		if v == nil {
			delete(doc, k)
		} else {
			doc[k] = v
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// registerError registers a body that must be refused and checks the RFC 7591
// error code.
func registerError(t *testing.T, f *fixture, body, wantCode string) {
	t.Helper()
	resp := f.register(t, "application/json", "203.0.113.11:4000", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["error"] != wantCode {
		t.Fatalf("error = %q, want %q", out["error"], wantCode)
	}
}

// TestRegisteredClientRunsFullFlow registers a client over HTTP and then
// drives authorize, consent and token exchange with the issued client_id.
func TestRegisteredClientRunsFullFlow(t *testing.T) {
	f := newFixture(t)
	cid := f.registerOK(t, registrationBody(nil))
	if !strings.HasPrefix(cid, registerClientIDPfx) {
		t.Fatalf("client_id = %q, want the dynamic prefix", cid)
	}
	redirect := f.approveAuthorize(map[string]string{"client_id": cid})
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeFrom(t, redirect)},
		"client_id":     {cid},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	})
	if status != http.StatusOK {
		t.Fatalf("exchange status = %d body=%v", status, body)
	}
	refresh, _ := body["refresh_token"].(string)
	if refresh == "" {
		t.Fatal("default grants include refresh_token but none issued")
	}
}

func TestRegisterResponseIsNoStoreWithIssuedFields(t *testing.T) {
	f := newFixture(t)
	resp := f.register(t, "application/json", "203.0.113.12:4000", registrationBody(nil))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["client_secret"]; ok {
		t.Fatal("registration issued a client_secret")
	}
	if out["token_endpoint_auth_method"] != "none" {
		t.Fatalf("token_endpoint_auth_method = %v", out["token_endpoint_auth_method"])
	}
	if out["client_id_issued_at"] == nil {
		t.Fatal("client_id_issued_at missing")
	}
}

func TestRegisterRejectsBadRedirectURIs(t *testing.T) {
	f := newFixture(t)
	for name, uri := range map[string]string{
		"plain http non-loopback": "http://client.example/cb",
		"fragment":                "https://client.example/cb#x",
		"wildcard":                "https://*.example/cb",
	} {
		t.Run(name, func(t *testing.T) {
			registerError(t, f, registrationBody(map[string]any{"redirect_uris": []string{uri}}), "invalid_redirect_uri")
		})
	}
}

func TestRegisterRejectsEmptyRedirectURIs(t *testing.T) {
	f := newFixture(t)
	registerError(t, f, registrationBody(map[string]any{"redirect_uris": []string{}}), "invalid_redirect_uri")
}

func TestRegisterRejectsSecretAuthMethod(t *testing.T) {
	f := newFixture(t)
	registerError(t, f, registrationBody(map[string]any{"token_endpoint_auth_method": "client_secret_basic"}), "invalid_client_metadata")
}

func TestRegisterRejectsUnsupportedGrantAndResponseTypes(t *testing.T) {
	f := newFixture(t)
	registerError(t, f, registrationBody(map[string]any{"grant_types": []string{"client_credentials"}}), "invalid_client_metadata")
	registerError(t, f, registrationBody(map[string]any{"response_types": []string{"token"}}), "invalid_client_metadata")
}

func TestRegisterRejectsUnknownApplicationType(t *testing.T) {
	f := newFixture(t)
	registerError(t, f, registrationBody(map[string]any{"application_type": "service"}), "invalid_client_metadata")
}

func TestRegisterRejectsNonJSON(t *testing.T) {
	f := newFixture(t)
	resp := f.register(t, "text/plain", "203.0.113.13:4000", registrationBody(nil))
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
	resp = f.register(t, "application/json", "203.0.113.13:4000", "not json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed JSON status = %d, want 400", resp.StatusCode)
	}
}

func TestRegisterRejectsOversizedBody(t *testing.T) {
	f := newFixture(t)
	big := registrationBody(map[string]any{"client_name": strings.Repeat("a", 17<<10)})
	if len(big) <= registerMaxBody {
		t.Fatal("test body is not oversized")
	}
	resp := f.register(t, "application/json", "203.0.113.14:4000", big)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

// TestRegisterIgnoresUnknownMetadata: RFC 7591 section 2 requires unknown
// members to be ignored, not refused.
func TestRegisterIgnoresUnknownMetadata(t *testing.T) {
	f := newFixture(t)
	body := registrationBody(map[string]any{"software_id": "abc", "x_future_field": []int{1, 2}})
	resp := f.register(t, "application/json", "203.0.113.15:4000", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("unknown metadata status = %d, want 201", resp.StatusCode)
	}
}

// TestRegisterRateLimitThenRecovery: the 21st registration from one IP inside
// the window gets 429 with Retry-After, and the window reopens afterwards.
func TestRegisterRateLimitThenRecovery(t *testing.T) {
	f := newFixture(t)
	const ip = "198.51.100.20:4000"
	for i := 0; i < registerPerWindow; i++ {
		resp := f.register(t, "application/json", ip, registrationBody(nil))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("registration %d status = %d", i+1, resp.StatusCode)
		}
	}
	resp := f.register(t, "application/json", ip, registrationBody(nil))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-limit status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	// A different IP is not affected by this one's limit.
	other := f.register(t, "application/json", "198.51.100.21:4000", registrationBody(nil))
	if other.StatusCode != http.StatusCreated {
		t.Fatalf("other IP status = %d, want 201", other.StatusCode)
	}
	f.clock.Advance(registerWindow + time.Second)
	resp = f.register(t, "application/json", ip, registrationBody(nil))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("after window status = %d, want 201", resp.StatusCode)
	}
}

// TestRegisterIgnoresForwardedForForRateLimit: the limit keys on RemoteAddr,
// so a spoofed X-Forwarded-For does not reset it.
func TestRegisterIgnoresForwardedForForRateLimit(t *testing.T) {
	f := newFixture(t)
	const ip = "198.51.100.30:4000"
	for i := 0; i < registerPerWindow; i++ {
		_ = f.register(t, "application/json", ip, registrationBody(nil))
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(registrationBody(nil)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "192.0.2.99")
	req.RemoteAddr = ip
	rec := httptest.NewRecorder()
	f.srv.handleRegister(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed XFF bypassed the limit: status = %d", rec.Code)
	}
}

// TestRegisterGlobalCapRefusesNewClients uses a cap of 2 through newServer.
func TestRegisterGlobalCapRefusesNewClients(t *testing.T) {
	f := newFixture(t)
	f.serve(newServer(f.cfg, f.store, registeredResolver{f.store}, f.clock.Now, slog.New(slog.DiscardHandler), 2))
	f.registerOK(t, registrationBody(nil))
	f.registerOK(t, registrationBody(nil))
	resp := f.register(t, "application/json", "203.0.113.40:4000", registrationBody(nil))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-cap status = %d, want 503", resp.StatusCode)
	}
}

// TestRegisterWritesAuditRowWithoutIP: the audit row names the action and the
// new client, and carries no caller address.
func TestRegisterWritesAuditRowWithoutIP(t *testing.T) {
	f := newFixture(t)
	cid := f.registerOK(t, registrationBody(nil))
	n, err := f.store.CountAuditEvents(f.ctx, "oauth.client.register", "success")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
	stored, err := f.store.GetOAuthClient(f.ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Source != "dynamic" || bytes.Contains([]byte(stored.RedirectURIs), []byte("203.0.113")) {
		t.Fatalf("stored client = %+v", stored)
	}
}

// TestRegistrationKeyGroupsIPv6ByPrefix: an IPv6 client controls a whole /64,
// so the limit keys on that prefix. Addresses in one /64 share a budget and
// addresses in another /64 do not. IPv4 keeps one key per address.
func TestRegistrationKeyGroupsIPv6ByPrefix(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		want       string
	}{
		{"ipv6 host bits ignored", "[2001:db8:1:2::aaaa]:4000", "2001:db8:1:2::/64"},
		{"same /64, other host", "[2001:db8:1:2:ffff:ffff:ffff:ffff]:4000", "2001:db8:1:2::/64"},
		{"ipv4 keeps full address", "198.51.100.7:4000", "198.51.100.7"},
		{"ipv4-mapped ipv6 is ipv4", "[::ffff:198.51.100.7]:4000", "198.51.100.7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/oauth/register", nil)
			req.RemoteAddr = tc.remoteAddr
			if got := registrationClientIP(req, nil); got != tc.want {
				t.Fatalf("key = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRegisterLimitSharedAcrossIPv6Prefix: exhausting the budget from one /64
// also limits another address in that /64, but not an address elsewhere.
func TestRegisterLimitSharedAcrossIPv6Prefix(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < registerPerWindow; i++ {
		resp := f.register(t, "application/json", "[2001:db8:7:7::1]:4000", registrationBody(nil))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("registration %d status = %d", i+1, resp.StatusCode)
		}
	}
	sibling := f.register(t, "application/json", "[2001:db8:7:7:dead:beef::2]:4000", registrationBody(nil))
	if sibling.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("same /64 sibling status = %d, want 429", sibling.StatusCode)
	}
	elsewhere := f.register(t, "application/json", "[2001:db8:7:8::1]:4000", registrationBody(nil))
	if elsewhere.StatusCode != http.StatusCreated {
		t.Fatalf("different /64 status = %d, want 201", elsewhere.StatusCode)
	}
}
