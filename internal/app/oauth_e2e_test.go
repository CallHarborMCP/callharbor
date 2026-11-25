package app

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// TestOAuthE2EUnauthenticatedChallengeAndDiscovery follows the discovery path a
// remote MCP client takes: 401 challenge, protected resource metadata, then
// authorization server metadata.
func TestOAuthE2EUnauthenticatedChallengeAndDiscovery(t *testing.T) {
	e := startE2E(t, t.TempDir())

	status, hdr, body := send(t, e.client, http.MethodPost, e.resource(), []byte(`{}`),
		http.Header{"Content-Type": {"application/json"}})
	if status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /mcp = %d %s", status, body)
	}
	challenge := hdr.Get("WWW-Authenticate")
	if strings.Contains(challenge, "error=") {
		t.Fatalf("no-credentials challenge carries an error: %q", challenge)
	}
	if !strings.Contains(challenge, `scope="mcp:read mcp:call"`) {
		t.Fatalf("no-credentials challenge missing scope: %q", challenge)
	}
	m := resourceMetadataRE.FindStringSubmatch(challenge)
	if m == nil || m[1] != e.base+"/.well-known/oauth-protected-resource/mcp" {
		t.Fatalf("resource_metadata in %q", challenge)
	}

	_, _, prmBody := send(t, e.client, http.MethodGet, m[1], nil, nil)
	prm := decodeObject(t, prmBody)
	servers, _ := prm["authorization_servers"].([]any)
	if prm["resource"] != e.resource() || len(servers) != 1 || servers[0] != e.base {
		t.Fatalf("protected resource metadata = %v", prm)
	}

	_, _, asBody := send(t, e.client, http.MethodGet, e.base+"/.well-known/oauth-authorization-server", nil, nil)
	as := decodeObject(t, asBody)
	checks := map[string]bool{
		"issuer":                                         as["issuer"] == e.base,
		"registration_endpoint":                          as["registration_endpoint"] == e.base+"/oauth/register",
		"client_id_metadata_document_supported":          as["client_id_metadata_document_supported"] == true,
		"authorization_response_iss_parameter_supported": as["authorization_response_iss_parameter_supported"] == true,
		"S256": jsonHas(as["code_challenge_methods_supported"], "S256"),
	}
	for name, ok := range checks {
		if !ok {
			t.Fatalf("authorization server metadata %s wrong: %v", name, as)
		}
	}
}

// TestOAuthE2EHappyPath runs the full authorization code flow through
// x/oauth2 and checks the consent-API rules along the way.
func TestOAuthE2EHappyPath(t *testing.T) {
	e := startE2E(t, t.TempDir())
	alice := createUser(t, e.admin, "alice")
	redirect := callbackURL(t)
	cid := e.register(redirect)

	verifier := oauth2.GenerateVerifier()
	conf := e.oauthConfig(cid, redirect, "mcp:read", "mcp:call")
	reqID := e.startAuthorize(conf, verifier)

	if status, _, _ := send(t, e.client, http.MethodGet, e.base+"/api/oauth/requests/"+reqID, nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("consent view without session = %d, want 401", status)
	}
	view := e.consentView(alice, reqID)
	if view["client_name"] != "e2e" || view["resource"] != e.resource() || view["redirect_uri"] != redirect {
		t.Fatalf("consent view = %v", view)
	}
	if status, _, _ := sendJSON(t, alice.client, http.MethodPost,
		e.base+"/api/oauth/requests/"+reqID+"/decision", map[string]bool{"approve": true}, alice.headers(false)); status != http.StatusForbidden {
		t.Fatalf("decision without CSRF = %d, want 403", status)
	}

	tok := e.exchange(conf, verifier, e.decide(alice, reqID, true))
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatal("exchange returned no access or refresh token")
	}

	status, _, body := e.mcpWith(tok.AccessToken)
	if status != http.StatusNotImplemented {
		t.Fatalf("/mcp with bearer = %d %s, want 501", status, body)
	}
	rpcErr, _ := decodeObject(t, body)["error"].(map[string]any)
	if rpcErr["code"] != float64(-32601) {
		t.Fatalf("/mcp error = %v", rpcErr)
	}
}

// TestOAuthE2EDenyRedirectsWithAccessDenied checks that a refusal still returns
// state and iss to the client.
func TestOAuthE2EDenyRedirectsWithAccessDenied(t *testing.T) {
	e := startE2E(t, t.TempDir())
	alice := createUser(t, e.admin, "alice")
	redirect := callbackURL(t)
	conf := e.oauthConfig(e.register(redirect), redirect, "mcp:read")
	reqID := e.startAuthorize(conf, oauth2.GenerateVerifier())

	q := e.decide(alice, reqID, false).Query()
	if q.Get("error") != "access_denied" || q.Get("state") != e2eState || q.Get("iss") != e.base {
		t.Fatalf("deny redirect query = %v", q)
	}
}

// TestOAuthE2ECORSScope checks that CORS is granted only on the public OAuth
// endpoints, including the OPTIONS preflight, and never on the session API.
func TestOAuthE2ECORSScope(t *testing.T) {
	e := startE2E(t, t.TempDir())
	_ = createUser(t, e.admin, "alice")

	status, hdr, _ := send(t, e.client, http.MethodOptions, e.base+"/oauth/token", nil, http.Header{
		"Origin":                         {"https://client.example"},
		"Access-Control-Request-Method":  {"POST"},
		"Access-Control-Request-Headers": {"content-type, authorization"},
	})
	if status != http.StatusNoContent || hdr.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight /oauth/token = %d ACAO=%q", status, hdr.Get("Access-Control-Allow-Origin"))
	}

	_, hdr, _ = e.postForm(e.base+"/oauth/token", map[string][]string{
		"grant_type": {"refresh_token"}, "refresh_token": {"cho_r_none"}, "client_id": {"x"},
	})
	if hdr.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("/oauth/token response missing ACAO")
	}
	if hdr.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("/oauth/token sends credentials")
	}

	_, hdr, _ = send(t, e.client, http.MethodGet, e.base+"/api/session", nil, http.Header{"Origin": {"https://client.example"}})
	if hdr.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("/api/session carries CORS")
	}
}

// TestOAuthE2EConsentAPIRequiresSessionAndCSRF covers the consent API's
// authentication rules. The rejection happens before any request lookup.
func TestOAuthE2EConsentAPIRequiresSessionAndCSRF(t *testing.T) {
	e := startE2E(t, t.TempDir())
	alice := createUser(t, e.admin, "alice")
	if status, _, _ := send(t, e.client, http.MethodGet, e.base+"/api/oauth/grants", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("grants without session = %d, want 401", status)
	}
	if status, _, _ := send(t, e.client, http.MethodDelete, e.base+"/api/oauth/grants/g1", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("grant delete without session = %d, want 401", status)
	}
	if status, _, _ := send(t, alice.client, http.MethodDelete, e.base+"/api/oauth/grants/g1", nil, alice.headers(false)); status != http.StatusForbidden {
		t.Fatalf("grant delete without CSRF = %d, want 403", status)
	}
}

// TestOAuthE2EConsentPageRoute checks that /oauth/consent is served by its own
// route. With a built UI it returns HTML; without one it returns the 503 the
// explicit route gives, never a 404 or the OAuth handler.
func TestOAuthE2EConsentPageRoute(t *testing.T) {
	e := startE2E(t, t.TempDir())
	status, hdr, body := send(t, e.client, http.MethodGet, e.base+"/oauth/consent?request=cho_q_x", nil, nil)
	if _, err := os.Stat(filepath.Join("..", "..", "web", "static", "index.html")); err == nil {
		if status != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") || len(body) == 0 {
			t.Fatalf("/oauth/consent = %d %q", status, hdr.Get("Content-Type"))
		}
		return
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("/oauth/consent without UI = %d, want 503", status)
	}
}
