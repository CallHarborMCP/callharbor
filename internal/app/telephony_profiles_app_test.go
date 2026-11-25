package app

import (
	"net/http"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// TestTelephonyProfilesThroughRealApp boots the real app with a master key and
// exercises the telephony routes with a session and CSRF token. Create never
// validates and the profile list only reads the store, so no Telnyx request is
// made. The test asserts that the profile appears in /api/v1/profiles with an
// unknown connectivity status, and that the secret never leaves the server.
func TestTelephonyProfilesThroughRealApp(t *testing.T) {
	t.Setenv(secrets.EnvKey, validKeyB64())
	e := startE2E(t, t.TempDir())
	admin := e.admin
	const apiKey = "KEY-app-level-0123456789"

	status, _, body := sendJSON(t, admin.client, http.MethodPost, admin.base+"/api/v1/telephony-profiles",
		map[string]any{"name": "main", "caller_number": "+14155550100", "connection_id": "app-7", "api_key": apiKey},
		admin.headers(true))
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	created := decodeObject(t, body)
	id, _ := created["id"].(string)
	if id == "" || created["connectivity_status"] != "unknown" || created["access"] != "owner" {
		t.Fatalf("created profile = %v", created)
	}
	if strings.Contains(string(body), apiKey) {
		t.Fatalf("create response leaks the key: %s", body)
	}

	status, _, body = send(t, admin.client, http.MethodGet, admin.base+"/api/v1/telephony-profiles/"+id, nil, admin.headers(false))
	if status != http.StatusOK || strings.Contains(string(body), apiKey) {
		t.Fatalf("get = %d %s", status, body)
	}

	status, _, body = send(t, admin.client, http.MethodGet, admin.base+"/api/v1/profiles", nil, admin.headers(false))
	if status != http.StatusOK {
		t.Fatalf("/api/v1/profiles = %d %s", status, body)
	}
	if strings.Contains(string(body), apiKey) {
		t.Fatalf("/api/v1/profiles leaks the key: %s", body)
	}
	found := false
	for _, raw := range decodeObject(t, body)["telephony_profiles"].([]any) {
		p := raw.(map[string]any)
		if p["id"] == id {
			found = true
			if p["connectivity_status"] != "unknown" || p["access"] != "owner" {
				t.Fatalf("/api/v1/profiles entry = %v, want unknown owner", p)
			}
		}
	}
	if !found {
		t.Fatalf("/api/v1/profiles does not list %s: %s", id, body)
	}
}

// TestTelephonyProfileCreateWithoutMasterKeyIs503InRealApp runs the real app
// with no key and no stored secrets, so the unkeyed service is used. Create
// must fail closed with 503 and the internal envelope code.
func TestTelephonyProfileCreateWithoutMasterKeyIs503InRealApp(t *testing.T) {
	t.Setenv(secrets.EnvKey, "")
	t.Setenv(secrets.EnvKeyFile, "")
	e := startE2E(t, t.TempDir())
	admin := e.admin
	const placeholderKey = "KEY-x-0123456789" // gitleaks:allow
	status, _, body := sendJSON(t, admin.client, http.MethodPost, admin.base+"/api/v1/telephony-profiles",
		map[string]any{"name": "main", "caller_number": "+14155550100", "connection_id": "app-7", "api_key": placeholderKey},
		admin.headers(true))
	if status != http.StatusServiceUnavailable || !strings.Contains(string(body), `"code":"internal"`) {
		t.Fatalf("create without key = %d %s, want 503 internal", status, body)
	}
}

// TestTelnyxProviderRejectsEmptyKeyAndReturnsClient unit-tests the production
// provider factory. It never dials: it only builds the client.
func TestTelnyxProviderRejectsEmptyKeyAndReturnsClient(t *testing.T) {
	factory := telnyxProvider(telnyx.DefaultBaseURL)
	if _, err := factory(""); err == nil {
		t.Fatal("empty key built a provider")
	}
	p, err := factory("KEY-provider-0123456789")
	if err != nil {
		t.Fatalf("telnyxProvider: %v", err)
	}
	if _, ok := p.(*telnyx.Client); !ok {
		t.Fatalf("telnyxProvider returned %T, want *telnyx.Client", p)
	}
}
