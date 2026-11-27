package app

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// hangupURL is the REST hangup route for one call.
func hangupURL(env *callTestEnv, callID string) string {
	return env.base + "/api/v1/calls/" + callID + "/hangup"
}

// countHangupAudit counts call.hangup audit rows for one call with an outcome,
// and returns the target_user_id of the newest one.
func countHangupAudit(t *testing.T, dataDir, callID, outcome string) (int, string) {
	t.Helper()
	db := openAppDB(t, dataDir)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events
		WHERE action = 'call.hangup' AND resource_id = ? AND outcome = ?`, callID, outcome).Scan(&n); err != nil {
		t.Fatal(err)
	}
	var target string
	_ = db.QueryRow(`SELECT COALESCE(target_user_id, '') FROM audit_events
		WHERE action = 'call.hangup' AND resource_id = ? AND outcome = ? ORDER BY id DESC LIMIT 1`,
		callID, outcome).Scan(&target)
	return n, target
}

// startOwnerCall sets up alice's profile and AI profile, places one call and
// drives it to active over the signed webhooks. It returns the env, the fake
// provider, the AI fake and the call id.
func startOwnerCall(t *testing.T, ctl, key string) (*callTestEnv, *fakeTelnyx, *fakeAI, string, string) {
	t.Helper()
	tel := newFakeTelnyx(t, ctl)
	ai := newFakeAI(t, realtime.ProtocolRealtime)
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, ai.endpoint())
	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls",
		makeCallBody(tpID), withIdempotency(h, key))
	if status != http.StatusAccepted {
		t.Fatalf("make call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)
	closeMedia := goLive(t, env, callID, ctl)
	t.Cleanup(closeMedia)
	waitForCallStatus(t, env, h, callID, "active")
	return env, tel, ai, callID, tpID
}

// goLive drives a dialed call's media stream so its AI session starts. The
// media stream is what starts the AI session; without it there is no session
// for a hangup to close. The caller closes the returned connection.
func goLive(t *testing.T, env *callTestEnv, callID, ctl string) func() {
	t.Helper()
	env.signedWebhook(callID, "call.initiated", "evt-live-"+callID+"-1", ctl)
	env.signedWebhook(callID, "call.answered", "evt-live-"+callID+"-2", ctl)
	mediaConn, err := env.signedMediaDial(env.dialedStreamURL())
	if err != nil {
		t.Fatalf("media dial: %v", err)
	}
	sendStart(t, mediaConn, ctl)
	return func() { _ = mediaConn.Close() }
}

// TestOwnerHangupActiveCallClosesAISessionAndEnds covers the owner path on a
// live call: REST hangup returns ending, the AI session closes, one owner-key
// hangup is sent, and the call ends on the signed call.hangup webhook.
func TestOwnerHangupActiveCallClosesAISessionAndEnds(t *testing.T) {
	const ctl = "v3:ctl-owner-hangup"
	env, tel, ai, callID, _ := startOwnerCall(t, ctl, "owner-hangup")
	h := env.admin.headers(true)

	status, _, body := send(t, env.client, http.MethodPost, hangupURL(env, callID), nil, h)
	if status != http.StatusOK {
		t.Fatalf("hangup = %d %s, want 200", status, body)
	}
	if got, _ := decodeObject(t, body)["technical_status"].(string); got != "ending" {
		t.Fatalf("hangup status = %q, want ending", got)
	}

	waitAISessionClosed(t, ai)
	waitHangups(t, tel, 1)
	assertOneOwnerKeyHangup(t, tel, ctl, callID)

	env.signedWebhook(callID, "call.hangup", "evt-owner-hangup-3", ctl)
	waitForCallStatus(t, env, h, callID, "ended")
	if reason := callReason(t, env, h, callID); reason != "hangup_requested" {
		t.Fatalf("outcome_reason_code = %q, want hangup_requested", reason)
	}
}

// TestAdminHangupOfAnotherUsersCallIsAuditedWithTarget covers the admin path:
// an administrator ends bob's call, and the audit row records bob as the target
// user with outcome success.
func TestAdminHangupOfAnotherUsersCallIsAuditedWithTarget(t *testing.T) {
	const ctl = "v3:ctl-admin-hangup"
	tel := newFakeTelnyx(t, ctl)
	ai := newFakeAI(t, realtime.ProtocolRealtime)
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	bob := createUser(t, env.admin, "bob")
	bh := bob.headers(true)
	createAIFixture(t, env, bh, realtime.ProtocolRealtime, ai.endpoint())
	grantTo(t, env, h, tpID, bob)

	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls",
		makeCallBody(tpID), withIdempotency(bh, "admin-hangup-bob"))
	if status != http.StatusAccepted {
		t.Fatalf("bob call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)
	closeMedia := goLive(t, env, callID, ctl)
	defer closeMedia()
	// Bob owns the call, so only his session can read its status.
	waitForCallStatus(t, env, bh, callID, "active")

	status, _, body = send(t, env.client, http.MethodPost, hangupURL(env, callID), nil, h)
	if status != http.StatusOK {
		t.Fatalf("admin hangup = %d %s, want 200", status, body)
	}
	waitHangups(t, tel, 1)

	n, target := countHangupAudit(t, env.dataDir, callID, "success")
	if n != 1 {
		t.Fatalf("call.hangup success rows = %d, want 1", n)
	}
	if target != bobID(t, bob) {
		t.Fatalf("audit target_user_id = %q, want bob %q", target, bobID(t, bob))
	}
}

// grantTo grants one user the telephony profile as the admin does.
func grantTo(t *testing.T, env *callTestEnv, h http.Header, profileID string, user *session) {
	t.Helper()
	status, _, body := send(t, env.client, http.MethodPut,
		env.base+"/api/v1/telephony-profiles/"+profileID+"/grants/"+bobID(t, user), nil, h)
	if status != http.StatusNoContent {
		t.Fatalf("grant = %d %s", status, body)
	}
}

// TestOtherUserHangupIsNotFoundAndNoStateChange covers the other-user path: bob
// cannot end alice's call. He gets not_found, the call is unchanged, and no
// hangup is sent.
func TestOtherUserHangupIsNotFoundAndNoStateChange(t *testing.T) {
	const ctl = "v3:ctl-other-hangup"
	env, tel, _, callID, _ := startOwnerCall(t, ctl, "other-hangup")
	bob := createUser(t, env.admin, "bob")

	status, _, body := send(t, env.client, http.MethodPost, hangupURL(env, callID), nil, bob.headers(true))
	if status != http.StatusNotFound {
		t.Fatalf("other user hangup = %d %s, want 404", status, body)
	}
	if strings.Contains(string(body), callID) {
		t.Fatalf("refusal echoes the call id: %s", body)
	}
	if got := tel.hangupsSeen(); len(got) != 0 {
		t.Fatalf("other user caused hangups %v", got)
	}
	if got := waitStatusNow(t, env, env.admin.headers(true), callID); got != "active" {
		t.Fatalf("call status = %q after refused hangup, want active", got)
	}
	if n, _ := countHangupAudit(t, env.dataDir, callID, "denied"); n != 1 {
		t.Fatalf("denied call.hangup rows = %d, want 1", n)
	}
}

// TestGranteeHangupUsesOwnerKey covers a grantee ending a call on the owner's
// profile: the leg is hung up once, with the profile owner's key, and the
// caller's grant is not re-checked.
func TestGranteeHangupUsesOwnerKey(t *testing.T) {
	const ctl = "v3:ctl-grantee-hangup"
	env, tel, callID, bob := startGranteeCallWithSession(t, ctl, "grantee-hangup")
	status, _, body := send(t, env.client, http.MethodPost, hangupURL(env, callID), nil, bob.headers(true))
	if status != http.StatusOK {
		t.Fatalf("grantee hangup = %d %s, want 200", status, body)
	}
	waitHangups(t, tel, 1)
	assertOneOwnerKeyHangup(t, tel, ctl, callID)
}

// TestRepeatedHangupSendsOneCommand covers idempotence: a second hangup on an
// ending call returns the current state and does not send a second command id.
func TestRepeatedHangupSendsOneCommand(t *testing.T) {
	const ctl = "v3:ctl-repeat-hangup"
	env, tel, _, callID, _ := startOwnerCall(t, ctl, "repeat-hangup")
	h := env.admin.headers(true)

	for i := 0; i < 3; i++ {
		status, _, body := send(t, env.client, http.MethodPost, hangupURL(env, callID), nil, h)
		if status != http.StatusOK {
			t.Fatalf("hangup %d = %d %s, want 200", i, status, body)
		}
	}
	// Every send carries the same command id. The spec allows a re-sent hangup
	// on an ending call, so the assertion is on distinct command ids, not sends.
	waitHangups(t, tel, 1)
	seen := tel.hangupsSeen()
	commands := map[string]bool{}
	for _, s := range seen {
		commands[s] = true
	}
	if len(commands) != 1 || !commands[ctl+"|"+legHangupCommandID(callID)] {
		t.Fatalf("hangups = %v, want one distinct command id for the leg", seen)
	}
}

// TestHangupQueuedCallNeverDials covers the queued path: the dial is held, the
// call is still queued, and the hangup fails it. When the held dial is released
// the worker finds the call failed, so Telnyx sees no dial for it.
func TestHangupQueuedCallNeverDials(t *testing.T) {
	const ctl = "v3:ctl-queued"
	tel := newFakeTelnyx(t, ctl)
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	release := tel.holdDials()
	defer release()
	// A first call occupies the held dial so the second stays queued.
	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls",
		makeCallBody(tpID), withIdempotency(h, "queued-first"))
	if status != http.StatusAccepted {
		t.Fatalf("first call = %d %s", status, body)
	}
	tel.waitDials(t, 1)

	status, _, body = sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls",
		makeCallBody(tpID), withIdempotency(h, "queued-second"))
	if status != http.StatusAccepted {
		t.Fatalf("second call = %d %s", status, body)
	}
	queuedID, _ := decodeObject(t, body)["call_id"].(string)

	status, _, body = send(t, env.client, http.MethodPost, hangupURL(env, queuedID), nil, h)
	if status != http.StatusOK {
		t.Fatalf("hangup queued = %d %s", status, body)
	}
	if got, _ := decodeObject(t, body)["technical_status"].(string); got != "failed" {
		t.Fatalf("queued hangup status = %q, want failed", got)
	}
	release()
	waitQuiet(tel)
	if n := len(tel.dialBodies()); n != 1 {
		t.Fatalf("dials = %d, want 1: the hung-up queued call must never dial", n)
	}
}

// TestHangupDuringInFlightDialSendsOneHangupToReturnedControlID covers the dial
// that is in flight when the hangup arrives. The hangup moves the call to
// ending with no control id. When the held dial returns its control id, the
// worker records it and sends exactly one hangup to that id.
func TestHangupDuringInFlightDialSendsOneHangupToReturnedControlID(t *testing.T) {
	const ctl = "v3:ctl-inflight"
	tel := newFakeTelnyx(t, ctl)
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	release := tel.holdDials()
	defer release()
	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls",
		makeCallBody(tpID), withIdempotency(h, "inflight"))
	if status != http.StatusAccepted {
		t.Fatalf("make call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)

	status, _, body = send(t, env.client, http.MethodPost, hangupURL(env, callID), nil, h)
	if status != http.StatusOK {
		t.Fatalf("in-flight hangup = %d %s", status, body)
	}
	if got, _ := decodeObject(t, body)["technical_status"].(string); got != "ending" {
		t.Fatalf("in-flight hangup status = %q, want ending", got)
	}
	if n := len(tel.hangupsSeen()); n != 0 {
		t.Fatalf("hangups before the dial returned = %d, want 0", n)
	}

	release()
	waitHangups(t, tel, 1)
	assertOneOwnerKeyHangup(t, tel, ctl, callID)
}

// TestMCPHangupCallEndToEnd covers the MCP tool: an mcp:call token ends the
// owner's active call, the AI session closes and one hangup is sent.
func TestMCPHangupCallEndToEnd(t *testing.T) {
	const ctl = "v3:ctl-mcp-hangup"
	env, tel, ai, callID, _ := startOwnerCall(t, ctl, "mcp-hangup")
	oe := env.oauthView()
	redirect := callbackURL(t)
	cid := oe.register(redirect)
	alice := env.admin
	tok := oe.grant(alice, cid, redirect, "mcp:read", "mcp:call").AccessToken
	sess := oe.sdkClient(t, tok)

	m := structuredMap(t, callTool(t, sess, "hangup_call", map[string]any{"call_id": callID}))
	if got, _ := m["technical_status"].(string); got != "ending" {
		t.Fatalf("hangup_call status = %v, want ending", m["technical_status"])
	}
	waitAISessionClosed(t, ai)
	waitHangups(t, tel, 1)
	assertOneOwnerKeyHangup(t, tel, ctl, callID)
}

// TestMCPHangupRequiresMCPCallScope covers the scope rule: a token with only
// mcp:read is refused with insufficient_scope and sends no hangup.
func TestMCPHangupRequiresMCPCallScope(t *testing.T) {
	const ctl = "v3:ctl-mcp-read"
	env, tel, _, callID, _ := startOwnerCall(t, ctl, "mcp-read-hangup")
	oe := env.oauthView()
	redirect := callbackURL(t)
	cid := oe.register(redirect)
	tok := oe.grant(env.admin, cid, redirect, "mcp:read").AccessToken
	sess := oe.sdkClient(t, tok)

	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "hangup_call", Arguments: map[string]any{"call_id": callID},
	})
	if err != nil {
		t.Fatalf("read-only hangup transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("read-only token ended a call: %+v", res)
	}
	if !strings.Contains(resultText(res), "insufficient_scope") {
		t.Fatalf("read-only refusal = %s, want insufficient_scope", resultText(res))
	}
	if n := len(tel.hangupsSeen()); n != 0 {
		t.Fatalf("read-only token caused %d hangups", n)
	}
}

// resultText returns the text content of a tool result.
func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// startGranteeCallWithSession is startGranteeCall, but it also returns the
// grantee's session, so a test can hang up as the grantee. It drives bob's call
// to active over the signed webhooks.
func startGranteeCallWithSession(t *testing.T, ctl, key string) (*callTestEnv, *fakeTelnyx, string, *session) {
	t.Helper()
	tel := newFakeTelnyx(t, ctl)
	ai := newFakeAI(t, realtime.ProtocolRealtime)
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	bob := createUser(t, env.admin, "bob")
	bh := bob.headers(true)
	createAIFixture(t, env, bh, realtime.ProtocolRealtime, ai.endpoint())
	grantTo(t, env, h, tpID, bob)

	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls",
		makeCallBody(tpID), withIdempotency(bh, key))
	if status != http.StatusAccepted {
		t.Fatalf("grantee call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)
	closeMedia := goLive(t, env, callID, ctl)
	t.Cleanup(closeMedia)
	waitForCallStatus(t, env, bh, callID, "active")
	return env, tel, callID, bob
}
