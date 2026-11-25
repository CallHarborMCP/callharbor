package app

import (
	"net/http"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// The three tests below place a grantee's call on the owner's telephony
// profile (R1). A grantee's leg is ended by the profile owner's key, found by
// the profile owner, not by the call owner. Each test expects exactly one
// hangup for the leg, sent with the owner's key.
//
// ownerKey is the API key createTelephonyFixture stores for the profile owner.
const ownerKey = "KEY-flow-0123456789"

// startGranteeCall sets up alice's profile, grants bob, places bob's call and
// drives it to active over the signed webhooks. It returns the env, the fake
// provider and the call id.
func startGranteeCall(t *testing.T, ctl, key string, protocol realtime.Protocol) (*callTestEnv, *fakeTelnyx, string) {
	t.Helper()
	tel := newFakeTelnyx(t, ctl)
	ai := newFakeAI(t, protocol)
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, protocol, newFakeAI(t, protocol).endpoint())

	bob := createUser(t, env.admin, "bob")
	bh := bob.headers(true)
	createAIFixture(t, env, bh, protocol, ai.endpoint())
	status, _, body := send(t, env.client, http.MethodPut, env.base+"/api/v1/telephony-profiles/"+tpID+"/grants/"+bobID(t, bob), nil, h)
	if status != http.StatusNoContent {
		t.Fatalf("grant bob = %d %s", status, body)
	}

	status, _, body = sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls",
		makeCallBody(tpID), withIdempotency(bh, key))
	if status != http.StatusAccepted {
		t.Fatalf("grantee call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)
	env.signedWebhook(callID, "call.initiated", "evt-"+key+"-1", ctl)
	env.signedWebhook(callID, "call.answered", "evt-"+key+"-2", ctl)
	waitForCallStatus(t, env, bh, callID, "active")
	return env, tel, callID
}

// assertOneOwnerKeyHangup checks the single hangup for the leg: one control id
// and command id, sent with the owner's bearer key.
func assertOneOwnerKeyHangup(t *testing.T, tel *fakeTelnyx, ctl, callID string) {
	t.Helper()
	got := tel.hangupsSeen()
	if len(got) != 1 || got[0] != ctl+"|"+legHangupCommandID(callID) {
		t.Fatalf("hangups = %v, want one for %s", got, ctl)
	}
	keys := tel.hangupKeysSeen()
	if len(keys) != 1 || keys[0] != ownerKey {
		t.Fatalf("hangup keys = %v, want the profile owner's key only", keys)
	}
}

// TestGranteeLegIsHungUpAtShutdownWithOwnersKey covers the shutdown path: a
// grantee's live leg is ended at shutdown with the profile owner's key.
func TestGranteeLegIsHungUpAtShutdownWithOwnersKey(t *testing.T) {
	for _, protocol := range aiFlowProtocols {
		t.Run(string(protocol), func(t *testing.T) {
			const ctl = "v3:ctl-grantee-sd"
			env, tel, callID := startGranteeCall(t, ctl, "grantee-sd", protocol)

			env.stop()

			assertOneOwnerKeyHangup(t, tel, ctl, callID)
		})
	}
}

// TestGranteeLegIsHungUpByCapWithOwnersKey covers the local end path: the
// max-duration cap ends a grantee's call, sending one hangup with the owner's
// key.
func TestGranteeLegIsHungUpByCapWithOwnersKey(t *testing.T) {
	for _, protocol := range aiFlowProtocols {
		t.Run(string(protocol), func(t *testing.T) {
			const ctl = "v3:ctl-grantee-cap"
			tel := newFakeTelnyx(t, ctl)
			ai := newFakeAI(t, protocol)
			env := startCallApp(t, tel, nil)
			h := env.admin.headers(true)
			tpID := createTelephonyFixture(t, env, h)
			createAIFixture(t, env, h, protocol, newFakeAI(t, protocol).endpoint())
			bob := createUser(t, env.admin, "bob")
			bh := bob.headers(true)
			createAIFixture(t, env, bh, protocol, ai.endpoint())
			status, _, body := send(t, env.client, http.MethodPut, env.base+"/api/v1/telephony-profiles/"+tpID+"/grants/"+bobID(t, bob), nil, h)
			if status != http.StatusNoContent {
				t.Fatalf("grant bob = %d %s", status, body)
			}
			status, _, body = sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
				"profile_id": tpID, "to": "+14155550111", "objective": "Confirm", "max_duration_seconds": 1,
			}, withIdempotency(bh, "grantee-cap"))
			if status != http.StatusAccepted {
				t.Fatalf("grantee cap call = %d %s", status, body)
			}
			callID, _ := decodeObject(t, body)["call_id"].(string)
			tel.waitDials(t, 1)
			env.signedWebhook(callID, "call.initiated", "evt-gc-1", ctl)
			env.signedWebhook(callID, "call.answered", "evt-gc-2", ctl)
			mediaConn, err := env.signedMediaDial(env.dialedStreamURL())
			if err != nil {
				t.Fatalf("media dial: %v", err)
			}
			defer func() { _ = mediaConn.Close() }()
			sendStart(t, mediaConn, ctl)

			waitAISessionClosed(t, ai)
			waitTechnicalStatus(t, env, bh, callID, "ending")
			waitHangups(t, tel, 1)
			if reason := callReason(t, env, bh, callID); reason != "max_duration" {
				t.Fatalf("outcome_reason_code = %q, want max_duration", reason)
			}
			assertOneOwnerKeyHangup(t, tel, ctl, callID)
		})
	}
}

// TestGranteeLegHangupSurvivesRevokedGrant covers the revoke path: bob's grant
// is revoked while his call is live. The local cap still ends the leg with the
// owner's key, so the revoke does not strand it.
func TestGranteeLegHangupSurvivesRevokedGrant(t *testing.T) {
	const ctl = "v3:ctl-grantee-rev"
	tel := newFakeTelnyx(t, ctl)
	ai := newFakeAI(t, realtime.ProtocolRealtime)
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	bob := createUser(t, env.admin, "bob")
	bh := bob.headers(true)
	createAIFixture(t, env, bh, realtime.ProtocolRealtime, ai.endpoint())
	bobUser := bobID(t, bob)
	status, _, body := send(t, env.client, http.MethodPut, env.base+"/api/v1/telephony-profiles/"+tpID+"/grants/"+bobUser, nil, h)
	if status != http.StatusNoContent {
		t.Fatalf("grant bob = %d %s", status, body)
	}
	status, _, body = sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": "Confirm", "max_duration_seconds": 1,
	}, withIdempotency(bh, "grantee-rev"))
	if status != http.StatusAccepted {
		t.Fatalf("grantee call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)
	env.signedWebhook(callID, "call.initiated", "evt-gr-1", ctl)
	env.signedWebhook(callID, "call.answered", "evt-gr-2", ctl)
	waitForCallStatus(t, env, bh, callID, "active")

	// Revoke mid-call. Bob can no longer use the profile, but his live leg is
	// still ours to end. Shutdown is the path that ends it, so the hangup
	// proves the revoked grant did not strand the leg.
	status, _, body = send(t, env.client, http.MethodDelete, env.base+"/api/v1/telephony-profiles/"+tpID+"/grants/"+bobUser, nil, h)
	if status != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", status, body)
	}
	status, _, body = sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls",
		makeCallBody(tpID), withIdempotency(bh, "grantee-rev-2"))
	if status != http.StatusNotFound {
		t.Fatalf("revoked grantee create = %d %s, want 404", status, body)
	}

	env.stop()
	assertOneOwnerKeyHangup(t, tel, ctl, callID)
}
