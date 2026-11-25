package app

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestOAuthE2ERevokeStopsMCP revokes an access token through /oauth/revoke on
// the running app, then checks that /mcp refuses it. The grant is revoked, so
// the 401 comes from revocation, which is the behaviour under test.
func TestOAuthE2ERevokeStopsMCP(t *testing.T) {
	e := startE2E(t, t.TempDir())
	alice := createUser(t, e.admin, "alice")
	redirect := callbackURL(t)
	cid := e.register(redirect)
	tok := e.grant(alice, cid, redirect, "mcp:read", "mcp:call")
	if status, _, _ := e.mcpWith(tok.AccessToken); status != http.StatusNotImplemented {
		t.Fatalf("/mcp before revoke = %d, want 501", status)
	}

	status, _, body := e.postForm(e.base+"/oauth/revoke", url.Values{
		"token":     {tok.AccessToken},
		"client_id": {cid},
	})
	if status != http.StatusOK {
		t.Fatalf("/oauth/revoke = %d %v", status, body)
	}

	status, hdr, _ := e.mcpWith(tok.AccessToken)
	if status != http.StatusUnauthorized || !strings.Contains(hdr.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("/mcp after revoke = %d %q, want 401 invalid_token", status, hdr.Get("WWW-Authenticate"))
	}
}
