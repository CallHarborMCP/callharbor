package oauth

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestMetadataAdvertisesOnlyImplementedCapabilities fetches both well-known
// documents over HTTP and checks every advertised value against what the
// server actually enforces.
func TestMetadataAdvertisesOnlyImplementedCapabilities(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/.well-known/oauth-authorization-server"} {
		// Single entry: the OIDC alias is removed (see TestOpenIDConfigurationIsNotServed).
		resp, err := http.Get(f.http.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", path, resp.StatusCode)
		}
		if doc["issuer"] != f.cfg.Issuer() {
			t.Fatalf("%s issuer = %v, want %q", path, doc["issuer"], f.cfg.Issuer())
		}
		if got := doc["code_challenge_methods_supported"]; !sameStrings(got, "S256") {
			t.Fatalf("%s code_challenge_methods_supported = %v, want [S256]", path, got)
		}
		if got := doc["grant_types_supported"]; !sameStrings(got, "authorization_code", "refresh_token") {
			t.Fatalf("%s grant_types_supported = %v", path, got)
		}
		if got := doc["token_endpoint_auth_methods_supported"]; !sameStrings(got, "none") {
			t.Fatalf("%s token_endpoint_auth_methods_supported = %v, want [none]", path, got)
		}
		if got := doc["response_types_supported"]; !sameStrings(got, "code") {
			t.Fatalf("%s response_types_supported = %v, want [code]", path, got)
		}
		if got := doc["scopes_supported"]; !sameStrings(got, "mcp:read", "mcp:call") {
			t.Fatalf("%s scopes_supported = %v", path, got)
		}
		if doc["revocation_endpoint"] != f.cfg.Issuer()+"/oauth/revoke" {
			t.Fatalf("%s revocation_endpoint = %v", path, doc["revocation_endpoint"])
		}
		if doc["registration_endpoint"] != f.cfg.Issuer()+"/oauth/register" {
			t.Fatalf("%s registration_endpoint = %v", path, doc["registration_endpoint"])
		}
		if doc["client_id_metadata_document_supported"] != true {
			t.Fatalf("%s must advertise client ID metadata document support", path)
		}
		if doc["authorization_response_iss_parameter_supported"] != true {
			t.Fatalf("%s must advertise RFC 9207 iss support", path)
		}
		if _, ok := doc["id_token_signing_alg_values_supported"]; ok {
			t.Fatalf("%s advertises ID token signing, which this server does not implement", path)
		}
	}
}

// TestOpenIDConfigurationIsNotServed: this server is not an OpenID Provider, so
// it must not answer the OIDC discovery path with an RFC 8414 document.
func TestOpenIDConfigurationIsNotServed(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Get(f.http.URL + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("openid-configuration status = %d, want 404", resp.StatusCode)
	}
}

// sameStrings reports whether got is a JSON array holding exactly want.
func sameStrings(got any, want ...string) bool {
	list, ok := got.([]any)
	if !ok || len(list) != len(want) {
		return false
	}
	for i, w := range want {
		if list[i] != w {
			return false
		}
	}
	return true
}
