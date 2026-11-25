package app

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestOAuthE2ERefreshRotationAndReuse rotates a refresh token, then replays the
// spent one. Reuse must return invalid_grant and revoke the family, so the newly
// issued access token is rejected too.
func TestOAuthE2ERefreshRotationAndReuse(t *testing.T) {
	e := startE2E(t, t.TempDir())
	alice := createUser(t, e.admin, "alice")
	redirect := callbackURL(t)
	cid := e.register(redirect)
	tok := e.grant(alice, cid, redirect, "mcp:read", "mcp:call")
	oldRefresh := tok.RefreshToken

	status, _, body := e.postForm(e.base+"/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {oldRefresh}, "client_id": {cid},
	})
	if status != http.StatusOK {
		t.Fatalf("refresh = %d %v", status, body)
	}
	newAccess, _ := body["access_token"].(string)
	newRefresh, _ := body["refresh_token"].(string)
	if newAccess == "" || newRefresh == "" || newRefresh == oldRefresh {
		t.Fatalf("refresh did not rotate: %v", body)
	}
	if status, _, b := e.mcpWith(newAccess); status != http.StatusOK {
		t.Fatalf("/mcp with rotated token = %d %v, want 200", status, b)
	}

	status, _, body = e.postForm(e.base+"/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {oldRefresh}, "client_id": {cid},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("reused refresh = %d %v, want 400 invalid_grant", status, body)
	}
	if status, hdr, _ := e.mcpWith(newAccess); status != http.StatusUnauthorized ||
		!strings.Contains(hdr.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("/mcp after family revocation = %d %q", status, hdr.Get("WWW-Authenticate"))
	}
}

// TestOAuthE2ERevokeGrantStopsMCP deletes a live grant through the consent API
// and expects the bearer to stop working at /mcp.
func TestOAuthE2ERevokeGrantStopsMCP(t *testing.T) {
	e := startE2E(t, t.TempDir())
	alice := createUser(t, e.admin, "alice")
	redirect := callbackURL(t)
	cid := e.register(redirect)
	tok := e.grant(alice, cid, redirect, "mcp:read", "mcp:call")

	gid := e.grantFor(alice, cid)
	if gid == "" {
		t.Fatal("no live grant listed for alice")
	}
	if status, _, body := send(t, alice.client, http.MethodDelete,
		e.base+"/api/oauth/grants/"+gid, nil, alice.headers(false)); status != http.StatusForbidden {
		t.Fatalf("grant delete without CSRF = %d %s, want 403", status, body)
	}
	if status, _, body := send(t, alice.client, http.MethodDelete,
		e.base+"/api/oauth/grants/"+gid, nil, alice.headers(true)); status != http.StatusNoContent {
		t.Fatalf("grant delete = %d %s, want 204", status, body)
	}
	if status, _, _ := e.mcpWith(tok.AccessToken); status != http.StatusUnauthorized {
		t.Fatalf("/mcp after grant delete = %d, want 401", status)
	}
	if e.grantFor(alice, cid) != "" {
		t.Fatal("revoked grant still listed")
	}
}

// TestOAuthE2ERevokeAnotherUsersGrantIs404 checks that one user cannot delete
// another user's grant. The response is 404 and the grant is left intact.
func TestOAuthE2ERevokeAnotherUsersGrantIs404(t *testing.T) {
	e := startE2E(t, t.TempDir())
	alice := createUser(t, e.admin, "alice")
	bob := createUser(t, e.admin, "bob")
	redirect := callbackURL(t)
	cid := e.register(redirect)
	e.grant(alice, cid, redirect, "mcp:read")
	gid := e.grantFor(alice, cid)

	if status, _, _ := send(t, bob.client, http.MethodDelete,
		e.base+"/api/oauth/grants/"+gid, nil, bob.headers(true)); status != http.StatusNotFound {
		t.Fatalf("bob deletes alice's grant = %d, want 404", status)
	}
	if e.grantFor(alice, cid) == "" {
		t.Fatal("alice's grant was changed by bob's request")
	}
}

// TestOAuthE2EDeactivationStopsMCP has the administrator deactivate the user
// and expects the user's live access token to fail at /mcp.
func TestOAuthE2EDeactivationStopsMCP(t *testing.T) {
	e := startE2E(t, t.TempDir())
	alice := createUser(t, e.admin, "alice")
	redirect := callbackURL(t)
	cid := e.register(redirect)
	tok := e.grant(alice, cid, redirect, "mcp:read", "mcp:call")
	if status, _, _ := e.mcpWith(tok.AccessToken); status != http.StatusOK {
		t.Fatalf("/mcp before deactivation = %d, want 200", status)
	}

	id := e.userID(alice)
	status, _, body := sendJSON(t, e.admin.client, http.MethodPatch, e.base+"/api/users/"+id,
		map[string]bool{"active": false}, e.admin.headers(true))
	if status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("deactivate = %d %s", status, body)
	}
	if status, hdr, _ := e.mcpWith(tok.AccessToken); status != http.StatusUnauthorized ||
		!strings.Contains(hdr.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("/mcp after deactivation = %d %q", status, hdr.Get("WWW-Authenticate"))
	}
}

// TestOAuthE2EAudienceMismatchRejected starts a second instance on the same
// data with a different origin. Instance A's token names A's /mcp, so B must
// reject it.
func TestOAuthE2EAudienceMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	a := startE2E(t, dir)
	alice := createUser(t, a.admin, "alice")
	redirect := callbackURL(t)
	cid := a.register(redirect)
	tok := a.grant(alice, cid, redirect, "mcp:read", "mcp:call")

	lnB := listen(t)
	baseB := "http://" + lnB.Addr().String()
	startInstance(t, dir, lnB, baseB)

	h := http.Header{"Authorization": {"Bearer " + tok.AccessToken}}
	status, hdr, body := sendJSON(t, a.client, http.MethodPost, baseB+e2eMCPPath,
		map[string]string{"jsonrpc": "2.0", "id": "1", "method": "ping"}, h)
	if status != http.StatusUnauthorized || !strings.Contains(hdr.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("instance B accepted A's token: %d %s", status, body)
	}
	if !strings.Contains(hdr.Get("WWW-Authenticate"), baseB+"/.well-known/oauth-protected-resource/mcp") {
		t.Fatalf("instance B challenge points elsewhere: %q", hdr.Get("WWW-Authenticate"))
	}
}

// TestOAuthE2EMCPCallOnlyIs403 issues a grant with only mcp:call. The /mcp
// route requires mcp:read, so the bearer gets 403 with the missing scope.
func TestOAuthE2EMCPCallOnlyIs403(t *testing.T) {
	e := startE2E(t, t.TempDir())
	alice := createUser(t, e.admin, "alice")
	redirect := callbackURL(t)
	cid := e.register(redirect)
	tok := e.grant(alice, cid, redirect, "mcp:call")

	status, hdr, body := e.mcpWith(tok.AccessToken)
	if status != http.StatusForbidden {
		t.Fatalf("call-only /mcp = %d %s, want 403", status, body)
	}
	h := hdr.Get("WWW-Authenticate")
	if !strings.Contains(h, `error="insufficient_scope"`) || !strings.Contains(h, `scope="mcp:read"`) {
		t.Fatalf("WWW-Authenticate = %q", h)
	}
}

// TestOAuthE2EDisabledWithoutPublicURL starts the app with no public origin.
// None of the OAuth or MCP routes may be served by the OAuth handler.
func TestOAuthE2EDisabledWithoutPublicURL(t *testing.T) {
	ln := listen(t)
	base := "http://" + ln.Addr().String()
	startInstance(t, t.TempDir(), ln, "")
	c := newE2EClient()

	status, hdr, body := send(t, c, http.MethodGet, base+"/oauth/authorize?client_id=x", nil, nil)
	if status == http.StatusFound && strings.Contains(hdr.Get("Location"), "/oauth/consent") {
		t.Fatal("/oauth/authorize served by the OAuth handler while disabled")
	}
	_ = body

	status, _, body = send(t, c, http.MethodGet, base+"/.well-known/oauth-authorization-server", nil, nil)
	if strings.Contains(string(body), "authorization_endpoint") {
		t.Fatalf("authorization server metadata served while disabled: %d", status)
	}

	status, _, body = send(t, c, http.MethodPost, base+e2eMCPPath, []byte(`{}`), nil)
	// With MCP unmounted, /mcp is not a route: 404 or 405 from the mux, never a
	// JSON-RPC reply or an authentication challenge from the OAuth layer.
	if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
		t.Fatalf("/mcp while disabled = %d, want 404 or 405", status)
	}
	if strings.Contains(string(body), "jsonrpc") || strings.Contains(string(body), "tools") {
		t.Fatalf("/mcp served while disabled: %d", status)
	}
}
