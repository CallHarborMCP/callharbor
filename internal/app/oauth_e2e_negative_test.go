package app

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// rawAuthorizeQuery is a complete /oauth/authorize request for cid. Tests edit
// it to drop or change one parameter. The client does not follow redirects, so
// the answer to each request is inspected directly.
func (e *e2eEnv) rawAuthorizeQuery(cid, redirect, verifier string) url.Values {
	return url.Values{
		"client_id":             {cid},
		"redirect_uri":          {redirect},
		"response_type":         {"code"},
		"state":                 {e2eState},
		"resource":              {e.resource()},
		"code_challenge":        {oauth2.S256ChallengeFromVerifier(verifier)},
		"code_challenge_method": {"S256"},
	}
}

// authorizeRaw sends a raw authorize request for the instance.
func (e *e2eEnv) authorizeRaw(q url.Values) (int, http.Header, []byte) {
	e.t.Helper()
	return send(e.t, e.client, http.MethodGet, e.base+"/oauth/authorize?"+q.Encode(), nil, nil)
}

// assertAuthorizeError requires an OAuth error redirected to redirect with the
// given error code, and no redirect to consent.
func assertAuthorizeError(t *testing.T, status int, hdr http.Header, body []byte, redirect, want string) {
	t.Helper()
	if status != http.StatusFound {
		t.Fatalf("authorize = %d %s, want a redirect carrying %s", status, body, want)
	}
	loc, err := url.Parse(hdr.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(loc.String(), redirect+"?") {
		t.Fatalf("error redirected to %s, want %s", loc, redirect)
	}
	if loc.Path == "/oauth/consent" {
		t.Fatal("refused request still reached consent")
	}
	if got := loc.Query().Get("error"); got != want {
		t.Fatalf("error = %q, want %q (%s)", got, want, loc)
	}
}

// codeFor runs authorize and consent approval for user and returns the code
// from the approval redirect. It does not redeem the code.
func (e *e2eEnv) codeFor(user *session, cid, redirect, verifier string) string {
	e.t.Helper()
	conf := e.oauthConfig(cid, redirect)
	reqID := e.startAuthorize(conf, verifier)
	e.consentView(user, reqID)
	code := e.decide(user, reqID, true).Query().Get("code")
	if code == "" {
		e.t.Fatal("approval redirect carries no code")
	}
	return code
}

// tokenFromCode redeems a code with the raw token endpoint and returns the
// status and decoded body, so a second redemption can be observed directly.
func (e *e2eEnv) tokenFromCode(cid, redirect, verifier, code string) (int, map[string]any) {
	e.t.Helper()
	status, _, body := e.postForm(e.base+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {cid},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
		"resource":      {e.resource()},
	})
	return status, body
}

// TestOAuthE2ECodeReuseRevokesIssuedTokens redeems one code twice. The second
// redemption must fail, and the access token from the first must stop working
// at /mcp, because replay revokes every token from that code.
func TestOAuthE2ECodeReuseRevokesIssuedTokens(t *testing.T) {
	e := startE2E(t, t.TempDir())
	alice := createUser(t, e.admin, "alice")
	redirect := callbackURL(t)
	cid := e.register(redirect)
	verifier := oauth2.GenerateVerifier()
	code := e.codeFor(alice, cid, redirect, verifier)

	status, body := e.tokenFromCode(cid, redirect, verifier, code)
	if status != http.StatusOK {
		t.Fatalf("first exchange = %d %v", status, body)
	}
	access, _ := body["access_token"].(string)
	if access == "" {
		t.Fatalf("first exchange returned no access token: %v", body)
	}
	if status, _, b := e.mcpWith(access); status != http.StatusOK {
		t.Fatalf("/mcp with first token = %d %v, want 200", status, b)
	}

	status, body = e.tokenFromCode(cid, redirect, verifier, code)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("code reuse = %d %v, want 400 invalid_grant", status, body)
	}
	if status, hdr, _ := e.mcpWith(access); status != http.StatusUnauthorized ||
		!strings.Contains(hdr.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("/mcp after code reuse = %d %q, want 401 invalid_token", status, hdr.Get("WWW-Authenticate"))
	}
}

// TestOAuthE2EMissingPKCERefused: without a code_challenge the request is
// refused at authorize. The error goes to the registered redirect, and no
// consent screen is reached.
func TestOAuthE2EMissingPKCERefused(t *testing.T) {
	e := startE2E(t, t.TempDir())
	redirect := callbackURL(t)
	cid := e.register(redirect)
	q := e.rawAuthorizeQuery(cid, redirect, oauth2.GenerateVerifier())
	q.Del("code_challenge")
	q.Del("code_challenge_method")

	status, hdr, body := e.authorizeRaw(q)
	assertAuthorizeError(t, status, hdr, body, redirect, "invalid_request")
}

// TestOAuthE2EPlainPKCERefused: the plain method is refused even with a
// well-formed challenge, so a client cannot skip the S256 hash.
func TestOAuthE2EPlainPKCERefused(t *testing.T) {
	e := startE2E(t, t.TempDir())
	redirect := callbackURL(t)
	cid := e.register(redirect)
	verifier := oauth2.GenerateVerifier()
	q := e.rawAuthorizeQuery(cid, redirect, verifier)
	q.Set("code_challenge", verifier)
	q.Set("code_challenge_method", "plain")

	status, hdr, body := e.authorizeRaw(q)
	assertAuthorizeError(t, status, hdr, body, redirect, "invalid_request")
}

// TestOAuthE2EUnregisteredRedirectNotRedirected: a redirect_uri the client did
// not register gets a JSON error and no Location header. Redirecting there
// would send an authorization response to an attacker-chosen URL. The
// candidates differ in host or path. A loopback URI may differ only in port
// (RFC 8252 section 7.3), so a port-only change is not an unregistered URI.
func TestOAuthE2EUnregisteredRedirectNotRedirected(t *testing.T) {
	e := startE2E(t, t.TempDir())
	cid := e.register(callbackURL(t))
	loopbackOtherPath := "http://" + listen(t).Addr().String() + "/other"
	for _, other := range []string{"https://attacker.example" + e2eCallbackPath, loopbackOtherPath} {
		q := e.rawAuthorizeQuery(cid, other, oauth2.GenerateVerifier())

		status, hdr, body := e.authorizeRaw(q)
		if status != http.StatusBadRequest {
			t.Fatalf("unregistered redirect_uri %s = %d %s, want 400", other, status, body)
		}
		if loc := hdr.Get("Location"); loc != "" {
			t.Fatalf("unregistered redirect_uri %s was redirected to %s", other, loc)
		}
		// The library's redirect validator answers invalid_request here. The
		// assertion that matters is the 400 with no Location above.
		if decodeObject(t, body)["error"] != "invalid_request" {
			t.Fatalf("body = %s, want invalid_request", body)
		}
	}
}

// TestOAuthE2EUnknownScopeIsInvalidScope: an unsupported scope is refused with
// invalid_scope on the registered redirect, before consent is reached.
func TestOAuthE2EUnknownScopeIsInvalidScope(t *testing.T) {
	e := startE2E(t, t.TempDir())
	redirect := callbackURL(t)
	cid := e.register(redirect)
	q := e.rawAuthorizeQuery(cid, redirect, oauth2.GenerateVerifier())
	q.Set("scope", "mcp:read mcp:admin")

	status, hdr, body := e.authorizeRaw(q)
	assertAuthorizeError(t, status, hdr, body, redirect, "invalid_scope")
}
