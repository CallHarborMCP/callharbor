package telnyx

import (
	"testing"
)

// costFixture is the call.cost example from the pinned Telnyx spec, with
// identifiers replaced by placeholders.
const costFixture = `{"data":{"record_type":"event","event_type":"call.cost","id":"0ccc7b54-4df3-4bca-a65a-3da1ecc777f0","occurred_at":"2018-02-02T22:25:27.521992Z","payload":{"call_control_id":"v3:CTL-PLACEHOLDER","call_leg_id":"LEG-PLACEHOLDER","call_session_id":"SESSION-PLACEHOLDER","client_state":"CLIENT-STATE-PLACEHOLDER","status":"success","total_cost":"0.0106","billed_duration_secs":120,"cost_parts":[{"call_part":"sip-trunking","rate":"0.0050","cost":"0.0050","currency":"USD","billed_duration_secs":60},{"call_part":"call-recording","rate":"0.00190","cost":"0.0038","currency":"USD","billed_duration_secs":120},{"call_part":"call-control","rate":"0.00180","cost":"0.0018","currency":"USD","billed_duration_secs":60}]}}}`

func TestParseCallCostSuccessFixture(t *testing.T) {
	ev, err := ParseWebhook([]byte(costFixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Type != EventCallCost || ev.Cost == nil {
		t.Fatalf("event = %+v, want a call.cost with cost set", ev)
	}
	if ev.Cost.Status != CostStatusSuccess || ev.Cost.TotalCost != "0.0106" {
		t.Fatalf("cost = %+v, want success 0.0106", ev.Cost)
	}
	if len(ev.Cost.Parts) != 3 || ev.Cost.Parts[0].Currency != "USD" {
		t.Fatalf("parts = %+v, want three USD parts", ev.Cost.Parts)
	}
	if _, mapped := ev.Signal(); mapped {
		t.Fatal("call.cost must not map to a lifecycle signal")
	}
}

func TestParseCallCostRefusesBadStatusAndAmount(t *testing.T) {
	bad := map[string]string{
		"status":         `"status":"success","total_cost":"0.0106"`,
		"unknown status": `"status":"pending","total_cost":"0.0106"`,
		"exponent":       `"status":"success","total_cost":"1e3"`,
		"negative":       `"status":"success","total_cost":"-1.00"`,
	}
	for name, frag := range bad {
		t.Run(name, func(t *testing.T) {
			body := `{"data":{"record_type":"event","event_type":"call.cost","id":"x","occurred_at":"2018-02-02T22:25:27Z","payload":{"call_control_id":"v3:c","client_state":"","` + frag + `,"cost_parts":[]}}}`
			if name == "status" {
				body = `{"data":{"record_type":"event","event_type":"call.cost","id":"x","occurred_at":"2018-02-02T22:25:27Z","payload":{"call_control_id":"v3:c","client_state":"","status":"","total_cost":"0.0106","cost_parts":[]}}}`
			}
			if _, err := ParseWebhook([]byte(body)); err == nil {
				t.Fatalf("%s accepted, want refusal", name)
			}
		})
	}
}

func TestParseCallCostErrorStatusAccepted(t *testing.T) {
	body := `{"data":{"record_type":"event","event_type":"call.cost","id":"e1","occurred_at":"2018-02-02T22:25:27Z","payload":{"call_control_id":"v3:c","client_state":"","status":"error","total_cost":null,"cost_parts":[]}}}`
	ev, err := ParseWebhook([]byte(body))
	if err != nil {
		t.Fatalf("parse error status: %v", err)
	}
	if ev.Cost == nil || ev.Cost.Status != CostStatusError || ev.Cost.TotalCost != "" {
		t.Fatalf("cost = %+v, want error with no total", ev.Cost)
	}
}
