package app

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// TestOversizeBriefIsRefusedWithoutRow checks that a brief over the configured
// byte limit is refused with brief_too_large, names the setting, and writes
// no call row. The brief is not truncated.
func TestOversizeBriefIsRefusedWithoutRow(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-big")
	env := startCallAppWith(t, tel, nil, func(c *config.Config) {
		c.MaxCallBriefBytes = 1024 // the configured floor
	})
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	// 19000 + 8000 runes is inside each field limit, but it exceeds the 1 KiB
	// byte limit, so the byte check refuses. The brief is never truncated.
	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": "Confirm",
		"context": strings.Repeat("c", 19000), "instructions": strings.Repeat("i", 8000),
	}, withIdempotency(h, "big-1"))
	if status == http.StatusAccepted {
		t.Fatalf("oversize brief was accepted: %s", body)
	}
	if !strings.Contains(string(body), "brief_too_large") || !strings.Contains(string(body), "CALLHARBOR_MAX_CALL_BRIEF_BYTES") {
		t.Fatalf("refusal = %d %s, want brief_too_large naming the setting", status, body)
	}
	if n := countCallRows(t, env.dataDir); n != 0 {
		t.Fatalf("oversize brief wrote %d call rows", n)
	}
	if n := len(tel.dialBodies()); n != 0 {
		t.Fatalf("oversize brief reached Telnyx with %d dials", n)
	}
}

// TestMakeCallAIProfileMissingOrAmbiguousIsRefused checks AI selection over
// REST: no AI profile is ai_profile_required, two are ai_profile_ambiguous, and
// neither writes a row.
func TestMakeCallAIProfileMissingOrAmbiguousIsRefused(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-ai")
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)

	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": "Confirm",
	}, withIdempotency(h, "ai-none"))
	if status == http.StatusAccepted || !strings.Contains(string(body), "ai_profile_required") {
		t.Fatalf("no AI profile = %d %s, want ai_profile_required", status, body)
	}

	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	status, _, body = sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": "Confirm",
	}, withIdempotency(h, "ai-two"))
	if status == http.StatusAccepted || !strings.Contains(string(body), "ai_profile_ambiguous") {
		t.Fatalf("two AI profiles = %d %s, want ai_profile_ambiguous", status, body)
	}
	if n := countCallRows(t, env.dataDir); n != 0 {
		t.Fatalf("refused AI selection wrote %d call rows", n)
	}
	if n := len(tel.dialBodies()); n != 0 {
		t.Fatalf("refused AI selection reached Telnyx with %d dials", n)
	}
}

// TestWebhookRejectionsChangeNoState checks the three webhook refusals through
// the real route: unsigned, signed with a different key, and a replayed event.
// Only the first accepted delivery changes state; the rejected ones do not.
func TestWebhookRejectionsChangeNoState(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-hook")
	env := startCallApp(t, tel, nil)
	h := env.admin.headers(true)
	tpID := createTelephonyFixture(t, env, h)
	createAIFixture(t, env, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	status, _, body := sendJSON(t, env.client, http.MethodPost, env.base+"/api/v1/calls", map[string]any{
		"profile_id": tpID, "to": "+14155550111", "objective": "Confirm",
	}, withIdempotency(h, "hook-1"))
	if status != http.StatusAccepted {
		t.Fatalf("create = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)

	ev := []byte(`{"data":{"record_type":"event","id":"evt-h1","event_type":"call.initiated","occurred_at":"2026-11-25T10:00:00Z","payload":{"call_control_id":"v3:ctl-hook"}}}`)
	url := env.base + "/telnyx/calls/" + callID + "/webhook"

	// Unsigned.
	un := http.Header{"Content-Type": {"application/json"}}
	if st, _, _ := send(t, env.client, http.MethodPost, url, ev, un); st != http.StatusUnauthorized {
		t.Fatalf("unsigned = %d, want 401", st)
	}
	// Wrong key: signed by a key the telephony profile does not hold.
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrong := telnyx.Sign(otherPriv, time.Now(), ev)
	wrong.Set("Content-Type", "application/json")
	if st, _, _ := send(t, env.client, http.MethodPost, url, ev, wrong); st != http.StatusUnauthorized {
		t.Fatalf("wrong key = %d, want 401", st)
	}
	// The dial already moved the call to initiating; a rejected delivery must
	// leave that status exactly as it was.
	if got := waitStatusNow(t, env, h, callID); got != "initiating" {
		t.Fatalf("unsigned or wrong-key delivery changed state to %q", got)
	}

	// A good delivery applies once; the same delivery replayed does not apply again.
	env.signedWebhook(callID, "call.initiated", "evt-h1", "v3:ctl-hook")
	waitForCallStatus(t, env, h, callID, "ringing")
	replay := telnyx.Sign(env.privKey, time.Now(), ev)
	replay.Set("Content-Type", "application/json")
	if st, _, _ := send(t, env.client, http.MethodPost, url, ev, replay); st != http.StatusOK {
		t.Fatalf("replay = %d, want 200 as a no-op", st)
	}
	if got := waitStatusNow(t, env, h, callID); got != "ringing" {
		t.Fatalf("replayed webhook moved state to %q", got)
	}
}

// waitStatusNow reads the call's current technical status once.
func waitStatusNow(t *testing.T, env *callTestEnv, h http.Header, callID string) string {
	t.Helper()
	status, _, body := send(t, env.client, http.MethodGet, env.base+"/api/v1/calls/"+callID, nil, h)
	if status != http.StatusOK {
		t.Fatalf("get call = %d %s", status, body)
	}
	got, _ := decodeObject(t, body)["technical_status"].(string)
	return got
}
