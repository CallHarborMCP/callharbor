package app

import (
	"net/http"
	"testing"
)

// TestGranteeHangupAfterGrantRevokedUsesOwnerKey covers the revoked-grant path
// through the grantee's own REST hangup. Bob places a call on alice's profile,
// the admin revokes his grant while the call is live, and bob then hangs up.
// The hangup must still return 200, and exactly one hangup must reach Telnyx
// with the owner's key. A revoked grant must never strand a live leg.
func TestGranteeHangupAfterGrantRevokedUsesOwnerKey(t *testing.T) {
	const ctl = "v3:ctl-grantee-revoked-hangup"
	env, tel, callID, bob := startGranteeCallWithSession(t, ctl, "grantee-revoked-hangup")
	admin := env.admin.headers(true)
	bh := bob.headers(true)
	tpID := granteeProfileID(t, env, admin)
	bobUser := bobID(t, bob)

	status, _, body := send(t, env.client, http.MethodDelete,
		env.base+"/api/v1/telephony-profiles/"+tpID+"/grants/"+bobUser, nil, admin)
	if status != http.StatusNoContent {
		t.Fatalf("revoke bob = %d %s", status, body)
	}

	status, _, body = send(t, env.client, http.MethodPost, hangupURL(env, callID), nil, bh)
	if status != http.StatusOK {
		t.Fatalf("revoked grantee hangup = %d %s, want 200", status, body)
	}
	if got, _ := decodeObject(t, body)["technical_status"].(string); got != "ending" {
		t.Fatalf("hangup status = %q, want ending", got)
	}

	waitHangups(t, tel, 1)
	assertOneOwnerKeyHangup(t, tel, ctl, callID)
}

// granteeProfileID returns the id of the single telephony profile the admin
// created in startGranteeCallWithSession. The helper does not return it, so the
// test reads it back from the list endpoint.
func granteeProfileID(t *testing.T, env *callTestEnv, h http.Header) string {
	t.Helper()
	status, _, body := send(t, env.client, http.MethodGet, env.base+"/api/v1/telephony-profiles", nil, h)
	if status != http.StatusOK {
		t.Fatalf("list telephony profiles = %d %s", status, body)
	}
	items, _ := decodeObject(t, body)["telephony_profiles"].([]any)
	if len(items) != 1 {
		t.Fatalf("telephony profiles = %d, want 1: %s", len(items), body)
	}
	id, _ := items[0].(map[string]any)["id"].(string)
	if id == "" {
		t.Fatalf("profile has no id: %s", body)
	}
	return id
}
