package app

import (
	"net/http"
	"net/netip"
	"testing"
)

// proxyRegisterBudget mirrors registerPerWindow in internal/oauth/register.go.
// The app package cannot read that unexported constant. If the budget changes,
// this test fails at the first refusal and this value must be updated.
const proxyRegisterBudget = 20

// TestOAuthE2ETrustedProxiesReachOAuthServer starts the app through Run with
// CALLHARBOR_TRUSTED_PROXIES set to loopback. The test client connects from
// loopback, so the peer is trusted and X-Forwarded-For picks the client. Each
// forwarded client has its own registration budget. If the trusted list were
// not passed from config to the OAuth server, both forwarded clients would
// share the loopback peer's bucket, and the second one would be refused.
func TestOAuthE2ETrustedProxiesReachOAuthServer(t *testing.T) {
	e := startE2EWithProxies(t, t.TempDir(), []netip.Prefix{
		netip.MustParsePrefix("127.0.0.1/32"),
		netip.MustParsePrefix("::1/128"),
	})

	for i := 0; i < proxyRegisterBudget; i++ {
		if status := e.registerFrom("203.0.113.1"); status != http.StatusCreated {
			t.Fatalf("client 1 registration %d = %d, want 201", i+1, status)
		}
	}
	if status := e.registerFrom("203.0.113.1"); status != http.StatusTooManyRequests {
		t.Fatalf("client 1 over budget = %d, want 429", status)
	}
	if status := e.registerFrom("203.0.113.2"); status != http.StatusCreated {
		t.Fatalf("client 2 registration = %d, want 201: trusted proxy list did not reach the OAuth server", status)
	}
}

// startE2EWithProxies is startE2E with a trusted proxy list in the config.
func startE2EWithProxies(t *testing.T, dataDir string, trusted []netip.Prefix) *e2eEnv {
	t.Helper()
	ln := listen(t)
	base := "http://" + ln.Addr().String()
	startInstanceWithProxies(t, dataDir, ln, base, trusted)
	c := newE2EClient()
	return &e2eEnv{t: t, base: base, client: c, admin: setupAdmin(t, c, base)}
}

// registerFrom posts one minimal public-client registration with
// X-Forwarded-For set to xff, and returns the status.
func (e *e2eEnv) registerFrom(xff string) int {
	e.t.Helper()
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Forwarded-For", xff)
	body := `{"client_name":"proxy e2e","redirect_uris":["http://127.0.0.1:9/cb"],` +
		`"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"],` +
		`"response_types":["code"]}`
	status, _, _ := send(e.t, e.client, http.MethodPost, e.base+"/oauth/register", []byte(body), h)
	return status
}
