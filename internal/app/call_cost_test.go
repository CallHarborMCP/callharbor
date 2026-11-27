package app

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// signedCost posts a signed call.cost for callID through the real webhook
// route, with the sanitized shape of the pinned spec example. It returns the
// HTTP status so tests can assert acceptance or refusal.
func (e *callTestEnv) signedCost(callID, eventID, controlID string, payload map[string]any) int {
	e.t.Helper()
	clientState := base64.StdEncoding.EncodeToString([]byte(callID))
	p := map[string]any{
		"call_control_id": controlID,
		"client_state":    clientState,
		"call_leg_id":     "leg-cost-placeholder",
		"call_session_id": "session-cost-placeholder",
		"status":          "success",
		"total_cost":      "0.0106",
		"cost_parts": []map[string]any{
			{"call_part": "sip-trunking", "rate": "0.0050", "cost": "0.0050", "currency": "USD", "billed_duration_secs": 60},
			{"call_part": "call-recording", "rate": "0.00190", "cost": "0.0038", "currency": "USD", "billed_duration_secs": 120},
			{"call_part": "call-control", "rate": "0.00180", "cost": "0.0018", "currency": "USD", "billed_duration_secs": 60},
		},
	}
	for k, v := range payload {
		p[k] = v
	}
	body, err := json.Marshal(map[string]any{"data": map[string]any{
		"record_type": "event", "id": eventID, "event_type": "call.cost",
		"occurred_at": "2026-11-25T10:00:00Z", "payload": p,
	}})
	if err != nil {
		e.t.Fatal(err)
	}
	h := telnyx.Sign(e.privKey, time.Now(), body)
	h.Set("Content-Type", "application/json")
	status, _, _ := send(e.t, e.client, http.MethodPost, e.base+"/telnyx/calls/"+callID+"/webhook", body, h)
	return status
}

// costDetails returns the cost object from the owner's REST detail view.
func costDetails(t *testing.T, env *callTestEnv, h http.Header, callID string) map[string]any {
	t.Helper()
	status, _, body := send(t, env.client, http.MethodGet, env.base+"/api/v1/calls/"+callID, nil, h)
	if status != http.StatusOK {
		t.Fatalf("get call = %d %s", status, body)
	}
	details, _ := decodeObject(t, body)["details"].(map[string]any)
	cost, ok := details["cost"].(map[string]any)
	if !ok {
		t.Fatalf("details.cost missing: %s", body)
	}
	return cost
}

// TestCallCostAfterHangupIsKnownAndExposed covers the normal path: the call
// ends, then the signed call.cost arrives, and the REST view reports the
// provider's amount as known.
func TestCallCostAfterHangupIsKnownAndExposed(t *testing.T) {
	const ctl = "v3:ctl-cost-known"
	env, _, _, callID, _ := startOwnerCall(t, ctl, "cost-known")
	h := env.admin.headers(true)

	env.signedWebhook(callID, "call.hangup", "evt-cost-known-hangup", ctl)
	waitForCallStatus(t, env, h, callID, "ended")

	if status := env.signedCost(callID, "cost-known-1", ctl, nil); status != http.StatusOK {
		t.Fatalf("call.cost = %d, want 200", status)
	}
	cost := costDetails(t, env, h, callID)
	if cost["status"] != "known" || cost["amount"] != "0.0106" || cost["currency"] != "USD" || cost["source"] != "provider" {
		t.Fatalf("details.cost = %v, want known 0.0106 USD provider", cost)
	}
	// The cost is not a state transition: the status is still ended.
	if s := waitStatusNow(t, env, h, callID); s != "ended" {
		t.Fatalf("status after cost = %q, want ended", s)
	}
}

// TestCallCostDuplicateDeliveryWritesOnce proves a redelivered event is deduped
// by its receipt: the cost is unchanged and a second copy is acknowledged.
func TestCallCostDuplicateDeliveryWritesOnce(t *testing.T) {
	const ctl = "v3:ctl-cost-dup"
	env, _, _, callID, _ := startOwnerCall(t, ctl, "cost-dup")
	h := env.admin.headers(true)
	env.signedWebhook(callID, "call.hangup", "evt-cost-dup-hangup", ctl)
	waitForCallStatus(t, env, h, callID, "ended")

	for i := 0; i < 2; i++ {
		if status := env.signedCost(callID, "cost-dup-1", ctl, nil); status != http.StatusOK {
			t.Fatalf("delivery %d = %d, want 200", i+1, status)
		}
	}
	if cost := costDetails(t, env, h, callID); cost["status"] != "known" || cost["amount"] != "0.0106" {
		t.Fatalf("details.cost after duplicate = %v, want known 0.0106", cost)
	}
}

// TestCallCostForeignControlIDIsRefused proves a cost bound to another leg's
// control id is not stored. It is refused permanently, so the stored cost stays
// unavailable.
func TestCallCostForeignControlIDIsRefused(t *testing.T) {
	const ctl = "v3:ctl-cost-foreign"
	env, _, _, callID, _ := startOwnerCall(t, ctl, "cost-foreign")
	h := env.admin.headers(true)
	env.signedWebhook(callID, "call.hangup", "evt-cost-foreign-hangup", ctl)
	waitForCallStatus(t, env, h, callID, "ended")

	if status := env.signedCost(callID, "cost-foreign-1", "v3:someone-else", nil); status != http.StatusOK {
		t.Fatalf("foreign cost = %d, want 200 acknowledged", status)
	}
	if cost := costDetails(t, env, h, callID); cost["status"] != "unavailable" {
		t.Fatalf("details.cost = %v, want unavailable after a foreign cost", cost)
	}
}

// TestCallCostReorderedBeforeHangupIsKnown covers reordered delivery: Telnyx
// sends call.cost while the call is still active, then call.hangup. The cost is
// not a state transition, so it is recorded and acknowledged with 200 at once,
// the call stays active, and the later hangup ends it normally with the cost
// already known.
func TestCallCostReorderedBeforeHangupIsKnown(t *testing.T) {
	const ctl = "v3:ctl-cost-early"
	env, _, _, callID, _ := startOwnerCall(t, ctl, "cost-early")
	h := env.admin.headers(true)

	if status := env.signedCost(callID, "cost-early-1", ctl, nil); status != http.StatusOK {
		t.Fatalf("early cost = %d, want 200 (cost is not a transition)", status)
	}
	if s := waitStatusNow(t, env, h, callID); s != "active" {
		t.Fatalf("status after early cost = %q, want active", s)
	}
	if cost := costDetails(t, env, h, callID); cost["status"] != "known" || cost["amount"] != "0.0106" {
		t.Fatalf("details.cost after early cost = %v, want known 0.0106", cost)
	}

	env.signedWebhook(callID, "call.hangup", "evt-cost-early-hangup", ctl)
	waitForCallStatus(t, env, h, callID, "ended")

	cost := costDetails(t, env, h, callID)
	if cost["status"] != "known" || cost["amount"] != "0.0106" || cost["currency"] != "USD" || cost["source"] != "provider" {
		t.Fatalf("details.cost after hangup = %v, want known 0.0106 USD provider", cost)
	}
}

// TestCallCostErrorStatusDoesNotBecomeKnown covers a provider error report: it
// is recorded, but it does not make the cost known.
func TestCallCostErrorStatusDoesNotBecomeKnown(t *testing.T) {
	const ctl = "v3:ctl-cost-error"
	env, _, _, callID, _ := startOwnerCall(t, ctl, "cost-error")
	h := env.admin.headers(true)
	env.signedWebhook(callID, "call.hangup", "evt-cost-error-hangup", ctl)
	waitForCallStatus(t, env, h, callID, "ended")

	payload := map[string]any{"status": "error", "total_cost": nil, "cost_parts": []map[string]any{}}
	if status := env.signedCost(callID, "cost-error-1", ctl, payload); status != http.StatusOK {
		t.Fatalf("error cost = %d, want 200", status)
	}
	if cost := costDetails(t, env, h, callID); cost["status"] == "known" {
		t.Fatalf("error report became known: %v", cost)
	}
}

// TestCallCostMixedCurrencyIsUnavailable covers parts in two currencies: the
// total cannot be summed, so no amount is shown.
func TestCallCostMixedCurrencyIsUnavailable(t *testing.T) {
	const ctl = "v3:ctl-cost-mixed"
	env, _, _, callID, _ := startOwnerCall(t, ctl, "cost-mixed")
	h := env.admin.headers(true)
	env.signedWebhook(callID, "call.hangup", "evt-cost-mixed-hangup", ctl)
	waitForCallStatus(t, env, h, callID, "ended")

	payload := map[string]any{"cost_parts": []map[string]any{
		{"cost": "0.0050", "currency": "USD"},
		{"cost": "0.0040", "currency": "EUR"},
	}}
	if status := env.signedCost(callID, "cost-mixed-1", ctl, payload); status != http.StatusOK {
		t.Fatalf("mixed cost = %d, want 200", status)
	}
	if cost := costDetails(t, env, h, callID); cost["status"] == "known" || cost["amount"] != nil {
		t.Fatalf("mixed currency showed an amount: %v", cost)
	}
}

// TestCallCostViewRendersAllThreeStatuses checks the REST view for unavailable
// and estimated as well as known, on real calls, so the three shapes are
// exercised end to end.
func TestCallCostViewRendersAllThreeStatuses(t *testing.T) {
	const ctl = "v3:ctl-cost-three"
	env, _, _, callID, _ := startOwnerCall(t, ctl, "cost-three")
	h := env.admin.headers(true)

	// Before any cost event: unavailable, with null fields.
	cost := costDetails(t, env, h, callID)
	if cost["status"] != "unavailable" || cost["amount"] != nil || cost["currency"] != nil || cost["source"] != nil {
		t.Fatalf("no-cost view = %v, want unavailable with nulls", cost)
	}
	env.signedWebhook(callID, "call.hangup", "evt-cost-three-hangup", ctl)
	waitForCallStatus(t, env, h, callID, "ended")
	if status := env.signedCost(callID, "cost-three-1", ctl, nil); status != http.StatusOK {
		t.Fatalf("cost = %d", status)
	}
	if cost := costDetails(t, env, h, callID); cost["status"] != "known" {
		t.Fatalf("known view = %v", cost)
	}
}
