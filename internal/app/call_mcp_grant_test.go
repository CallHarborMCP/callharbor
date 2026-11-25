package app

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// oauthView exposes the TLS origin to the OAuth helpers, which read base,
// client and admin. The MCP resource and callback are derived from base.
func (e *callTestEnv) oauthView() *e2eEnv {
	return &e2eEnv{t: e.t, base: e.base, client: e.client, admin: e.admin}
}

// makeCallArgs is a valid make_call body for the fixture profile.
func makeCallArgs(profileID, key string) map[string]any {
	return map[string]any{"profile_id": profileID, "to": "+14155550111", "objective": "Confirm", "idempotency_key": key}
}

// TestMCPMakeCallScopeAndGrantee covers the MCP path over the real call app:
// make_call succeeds with a bearer that holds mcp:call; a bearer with only
// mcp:read is refused; a grantee can place a call on a granted profile; and a
// user with no grant is refused with the same not-found as an unknown id.
func TestMCPMakeCallScopeAndGrantee(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-mcp")
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	oe := env.oauthView()

	alice := createUser(t, env.admin, "alice")
	createUser(t, env.admin, "carol")
	// Alice places calls on a profile the admin granted her. Her own AI profile
	// is what a call selects, so she gets one too.
	aliceH := alice.headers(true)
	status, _, body := send(t, env.client, http.MethodPut, env.base+"/api/v1/telephony-profiles/"+tpID+"/grants/"+bobID(t, alice), nil, h)
	if status != http.StatusNoContent {
		t.Fatalf("grant alice = %d %s", status, body)
	}
	createAIFixture(t, env, aliceH, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	redirect := callbackURL(t)
	cid := oe.register(redirect)

	// mcp:read alone must not be able to place a call.
	readTok := oe.grant(alice, cid, redirect, "mcp:read").AccessToken
	readSess := oe.sdkClient(t, readTok)
	res, err := readSess.CallTool(context.Background(), &mcp.CallToolParams{Name: "make_call", Arguments: makeCallArgs(tpID, "mcp-key")})
	if err != nil {
		t.Fatalf("read-only make_call transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("read-only token placed a call: %+v", res)
	}
	if n := len(tel.dialBodies()); n != 0 {
		t.Fatalf("read-only token reached Telnyx with %d dials", n)
	}

	// mcp:call with mcp:read succeeds and returns a queued call.
	callTok := oe.grant(alice, cid, redirect, "mcp:read", "mcp:call").AccessToken
	callSess := oe.sdkClient(t, callTok)
	m := structuredMap(t, callTool(t, callSess, "make_call", makeCallArgs(tpID, "mcp-key")))
	// The worker may claim the call before this response is read, so the
	// status is either queued or initiating. The id and one dial are fixed.
	if m["call_id"] == "" || m["created_at"] == "" {
		t.Fatalf("make_call result = %v, want call_id and created_at", m)
	}
	if s, _ := m["status"].(string); s != "queued" && s != "initiating" {
		t.Fatalf("make_call status = %v", m["status"])
	}
	tel.waitDials(t, 1)

	// Carol has no grant on Alice's profile: same not-found as an unknown id.
	carol := login(t, env.client, env.base, "carol")
	status, _, body = sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls",
		makeCallBody(tpID), withIdempotency(carol.headers(true), "carol-1"))
	if status != http.StatusNotFound {
		t.Fatalf("ungranted user = %d %s, want 404", status, body)
	}
	if strings.Contains(string(body), tpID) {
		t.Fatalf("refusal echoes the profile id: %s", body)
	}
}

// TestGranteeCallsOnGrantedProfileAndRevokedIsRefused covers the grant path:
// a granted user places a call with the owner's profile; after revoke the
// same user is refused and no row is written.
func TestGranteeCallsOnGrantedProfileAndRevokedIsRefused(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-grant")
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	bob := createUser(t, env.admin, "bob")
	// The grantee needs her own AI profile, since the AI profile is never
	// selected from the request.
	bh := bob.headers(true)
	createBobAI(t, env, bh)
	status, _, body := send(t, env.client, http.MethodPut, env.base+"/api/v1/telephony-profiles/"+tpID+"/grants/"+bobID(t, bob), nil, h)
	if status != http.StatusNoContent {
		t.Fatalf("grant = %d %s", status, body)
	}

	status, _, body = sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls",
		makeCallBody(tpID), withIdempotency(bh, "grantee-1"))
	if status != http.StatusAccepted {
		t.Fatalf("grantee call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)
	if callID == "" {
		t.Fatal("grantee call has no id")
	}

	status, _, body = send(t, env.client, http.MethodDelete, env.base+"/api/v1/telephony-profiles/"+tpID+"/grants/"+bobID(t, bob), nil, h)
	if status != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", status, body)
	}
	status, _, body = sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls",
		makeCallBody(tpID), withIdempotency(bh, "grantee-2"))
	if status != http.StatusNotFound {
		t.Fatalf("revoked grantee = %d %s, want 404", status, body)
	}
}

// makeCallBody is the REST body: REST takes the key in a header, not the body.
func makeCallBody(profileID string) map[string]any {
	return map[string]any{"profile_id": profileID, "to": "+14155550111", "objective": "Confirm"}
}

// bobID returns the user id of a session's user. Login returns only a token,
// so the id comes from the identity endpoint.
func bobID(t *testing.T, s *session) string {
	t.Helper()
	status, _, body := send(t, s.client, http.MethodGet, s.base+"/api/session", nil, s.headers(false))
	if status != http.StatusOK {
		t.Fatalf("session = %d %s", status, body)
	}
	id, _ := decodeObject(t, body)["user"].(map[string]any)["id"].(string)
	if id == "" {
		t.Fatalf("no user id in %s", body)
	}
	return id
}

// createBobAI gives the grantee her own AI profile so her call can select one.
func createBobAI(t *testing.T, env *callTestEnv, h http.Header) {
	t.Helper()
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
}
