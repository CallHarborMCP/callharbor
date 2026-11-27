package app

import (
	"net/http"
	"testing"
)

// recAssertNoRecording checks that a call has no recordings row and that both
// recording reads answer not_found, the same answer as an unknown call.
func recAssertNoRecording(t *testing.T, env *callTestEnv, s *session, callID string) {
	t.Helper()
	if n := recRecordingRows(t, env.dataDir, callID); n != 0 {
		t.Fatalf("recordings rows = %d, want 0", n)
	}
	for _, path := range []string{"/recording", "/recording/audio"} {
		status, _, body := send(t, env.client, http.MethodGet, env.base+"/api/v1/calls/"+callID+path, nil, s.headers(false))
		if status != http.StatusNotFound {
			t.Fatalf("GET %s = %d %s, want 404", path, status, body)
		}
	}
}

// recCallRecordEffective reads the call's record flags over REST.
func recCallRecordEffective(t *testing.T, env *callTestEnv, s *session, callID string) (requested, effective bool) {
	t.Helper()
	status, _, body := send(t, env.client, http.MethodGet, env.base+"/api/v1/calls/"+callID, nil, s.headers(false))
	if status != http.StatusOK {
		t.Fatalf("get call = %d %s", status, body)
	}
	// The record flags sit under details in the call view.
	d, _ := decodeObject(t, body)["details"].(map[string]any)
	req, _ := d["record_requested"].(bool)
	eff, _ := d["record_effective"].(bool)
	return req, eff
}

// TestRecordingForgedOverrideREST: the owner has not opted in, so a request
// for record=true is accepted but not effective. No recording is made.
func TestRecordingForgedOverrideREST(t *testing.T) {
	for _, protocol := range aiFlowProtocols {
		t.Run(string(protocol), func(t *testing.T) {
			tel := newFakeTelnyx(t, recControlID)
			ai := newFakeAI(t, protocol)
			env := startCallApp(t, tel, nil)
			h := env.admin.headers(true)
			tpID := createTelephonyFixture(t, env, h)
			createAIFixture(t, env, h, protocol, ai.endpoint())

			callID := recCreateCall(t, env, tpID, "forge-rest-"+string(protocol), map[string]any{"record": true})
			req, eff := recCallRecordEffective(t, env, env.admin, callID)
			if !req || eff {
				t.Fatalf("record_requested=%v record_effective=%v, want true/false", req, eff)
			}
			recRunMedia(t, env, tel, ai, protocol, callID)
			recAssertNoRecording(t, env, env.admin, callID)
		})
	}
}

// TestRecordingForgedOverrideMCP: the same forged request through the MCP
// make_call tool. The owner has not opted in, so nothing is recorded.
func TestRecordingForgedOverrideMCP(t *testing.T) {
	for _, protocol := range aiFlowProtocols {
		t.Run(string(protocol), func(t *testing.T) {
			tel := newFakeTelnyx(t, recControlID)
			ai := newFakeAI(t, protocol)
			env := startCallApp(t, tel, nil)
			h := env.admin.headers(true)
			tpID := createTelephonyFixture(t, env, h)
			createAIFixture(t, env, h, protocol, ai.endpoint())
			oe := env.oauthView()

			alice := createUser(t, env.admin, "alice")
			status, _, body := send(t, env.client, http.MethodPut, env.base+"/api/v1/telephony-profiles/"+tpID+"/grants/"+bobID(t, alice), nil, h)
			if status != http.StatusNoContent {
				t.Fatalf("grant alice = %d %s", status, body)
			}
			createAIFixture(t, env, alice.headers(true), protocol, ai.endpoint())

			redirect := callbackURL(t)
			cid := oe.register(redirect)
			tok := oe.grant(alice, cid, redirect, "mcp:read", "mcp:call").AccessToken
			sess := oe.sdkClient(t, tok)

			args := makeCallArgs(tpID, "forge-mcp-"+string(protocol))
			args["record"] = true
			m := structuredMap(t, callTool(t, sess, "make_call", args))
			callID, _ := m["call_id"].(string)
			if callID == "" {
				t.Fatalf("make_call returned no call_id: %v", m)
			}
			req, eff := recCallRecordEffective(t, env, alice, callID)
			if !req || eff {
				t.Fatalf("record_requested=%v record_effective=%v, want true/false", req, eff)
			}
			recRunMedia(t, env, tel, ai, protocol, callID)
			recAssertNoRecording(t, env, alice, callID)

			// get_recording over MCP refuses the same way.
			if code := toolErrorCode(t, callTool(t, sess, "get_recording", map[string]any{"call_id": callID})); code != "not_found" {
				t.Fatalf("get_recording code = %q, want not_found", code)
			}
		})
	}
}

// TestRecordingOptOut: the owner has opted in, but the call asks for no
// recording. Nothing is recorded.
func TestRecordingOptOut(t *testing.T) {
	for _, protocol := range aiFlowProtocols {
		t.Run(string(protocol), func(t *testing.T) {
			tel := newFakeTelnyx(t, recControlID)
			ai := newFakeAI(t, protocol)
			env := startCallApp(t, tel, nil)
			h := env.admin.headers(true)
			tpID := createTelephonyFixture(t, env, h)
			createAIFixture(t, env, h, protocol, ai.endpoint())

			recSetPreference(t, env, env.admin, true, true)
			callID := recCreateCall(t, env, tpID, "optout-"+string(protocol), map[string]any{"record": false})
			req, eff := recCallRecordEffective(t, env, env.admin, callID)
			if req || eff {
				t.Fatalf("record_requested=%v record_effective=%v, want false/false", req, eff)
			}
			recRunMedia(t, env, tel, ai, protocol, callID)
			recAssertNoRecording(t, env, env.admin, callID)
		})
	}
}

// TestRecordingPreferenceControls covers who may change recording, over the
// live app: CSRF on PUT, the acknowledgement needed to turn recording on, the
// self-only scope, and the audit trail of an OFF to ON to OFF round trip.
func TestRecordingPreferenceControls(t *testing.T) {
	tel := newFakeTelnyx(t, recControlID)
	env := startCallApp(t, tel, nil)
	admin := env.admin
	pref := env.base + "/api/preferences/recording"

	// PUT without the CSRF header is refused.
	status, _, body := sendJSON(t, env.client, http.MethodPut, pref,
		map[string]any{"enabled": true, "acknowledged": true}, admin.headers(false))
	if status != http.StatusForbidden {
		t.Fatalf("PUT without CSRF = %d %s, want 403", status, body)
	}
	// Turning recording on without the acknowledgement is refused.
	status, _, body = sendJSON(t, env.client, http.MethodPut, pref,
		map[string]any{"enabled": true, "acknowledged": false}, admin.headers(true))
	if status != http.StatusBadRequest {
		t.Fatalf("ON without ack = %d %s, want 400", status, body)
	}

	// Another user is created. The admin cannot enable recording for her: the
	// admin user update refuses the recording field, and her own setting stays off.
	alice := createUser(t, env.admin, "alice")
	aliceID := env.oauthView().userID(alice)
	status, _, body = sendJSON(t, env.client, http.MethodPatch, env.base+"/api/users/"+aliceID,
		map[string]any{"role": "user", "active": true, "recording_opt_in": true}, admin.headers(true))
	if status != http.StatusBadRequest {
		t.Fatalf("admin PATCH with recording field = %d %s, want 400", status, body)
	}
	status, _, body = send(t, env.client, http.MethodGet, pref, nil, alice.headers(false))
	if status != http.StatusOK || decodeObject(t, body)["enabled"] != false {
		t.Fatalf("alice preference = %d %s, want enabled false", status, body)
	}

	// Admin OFF -> ON (with ack) -> OFF: two preference audit rows.
	recSetPreference(t, env, admin, true, true)
	recSetPreference(t, env, admin, false, false)
	if n := recCountPreferenceAudits(t, env.dataDir); n != 2 {
		t.Fatalf("recording.preference_changed audits = %d, want 2", n)
	}
}

// recCountPreferenceAudits counts recording.preference_changed rows.
func recCountPreferenceAudits(t *testing.T, dataDir string) int {
	t.Helper()
	var n int
	err := openAppDB(t, dataDir).QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'recording.preference_changed'`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
