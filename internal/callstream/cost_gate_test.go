package callstream

import (
	"database/sql"
	"net/http"
	"testing"
)

// costPayload is a successful call.cost payload in the shape Telnyx sends.
func costPayload() map[string]any {
	return map[string]any{
		"status":     "success",
		"total_cost": "0.0106",
		"cost_parts": []map[string]any{{"cost": "0.0106", "currency": "USD"}},
	}
}

// providerCost reads the stored provider cost columns for a call.
func providerCost(t *testing.T, db *sql.DB, id string) (status string, amount sql.NullInt64, currency string) {
	t.Helper()
	var st, cur sql.NullString
	if err := db.QueryRow(`SELECT provider_cost_status, provider_cost_amount_micros, provider_cost_currency
		FROM calls WHERE id = ?`, id).Scan(&st, &amount, &cur); err != nil {
		t.Fatal(err)
	}
	return st.String, amount, cur.String
}

// TestCostBeforeTerminalIsRecordedAndAcknowledged covers reordered delivery: a
// cost that arrives while the call is still active is stored, not retried, and
// the call's state does not change.
func TestCostBeforeTerminalIsRecordedAndAcknowledged(t *testing.T) {
	s, db := newWebhookStore(t)
	seedCall(t, db, "c-early", "active", "cc-early")
	f := newFixture(t, s)

	rec := f.post(t, "c-early", event(t, "c-early", "cost-early-1", "call.cost", "cc-early", costPayload()))
	if rec.Code != http.StatusOK {
		t.Fatalf("early cost = %d, want 200", rec.Code)
	}
	st, _, _ := callStatus(t, db, "c-early")
	if st != "active" {
		t.Fatalf("status after cost = %q, want active (cost is not a transition)", st)
	}
	status, amount, cur := providerCost(t, db, "c-early")
	if status != "success" || !amount.Valid || amount.Int64 != 10600 || cur != "USD" {
		t.Fatalf("provider cost = %q %v %q, want success 10600 USD", status, amount, cur)
	}
}

// TestCostThenHangupEndsCallWithKnownCost covers the reordered sequence end to
// end at the webhook layer: the cost lands first, then the hangup ends the call,
// and the stored cost is unchanged by the hangup.
func TestCostThenHangupEndsCallWithKnownCost(t *testing.T) {
	s, db := newWebhookStore(t)
	seedCall(t, db, "c-order", "active", "cc-order")
	f := newFixture(t, s)

	if rec := f.post(t, "c-order", event(t, "c-order", "cost-order-1", "call.cost", "cc-order", costPayload())); rec.Code != http.StatusOK {
		t.Fatalf("cost = %d, want 200", rec.Code)
	}
	if rec := f.post(t, "c-order", event(t, "c-order", "hangup-order-1", "call.hangup", "cc-order",
		map[string]any{"hangup_cause": "normal_clearing"})); rec.Code != http.StatusOK {
		t.Fatalf("hangup = %d, want 200", rec.Code)
	}
	st, _, _ := callStatus(t, db, "c-order")
	if st != "ended" {
		t.Fatalf("status = %q, want ended", st)
	}
	status, amount, _ := providerCost(t, db, "c-order")
	if status != "success" || !amount.Valid || amount.Int64 != 10600 {
		t.Fatalf("cost after hangup = %q %v, want success 10600", status, amount)
	}
}

// TestCostForNeverDialedCallIsRefusedAndStoresNothing keeps the lifecycle rule:
// a queued call has no leg, so a cost for it is refused permanently (200) and
// nothing is written.
func TestCostForNeverDialedCallIsRefusedAndStoresNothing(t *testing.T) {
	s, db := newWebhookStore(t)
	seedCall(t, db, "c-queued", "queued", "")
	f := newFixture(t, s)
	_, _, before := callStatus(t, db, "c-queued")

	rec := f.post(t, "c-queued", event(t, "c-queued", "cost-queued-1", "call.cost", "cc-ghost", costPayload()))
	if rec.Code != http.StatusOK {
		t.Fatalf("never-dialed cost = %d, want 200 (permanent refusal)", rec.Code)
	}
	status, amount, _ := providerCost(t, db, "c-queued")
	if status != "" || amount.Valid {
		t.Fatalf("never-dialed call stored a cost: %q %v", status, amount)
	}
	if _, _, after := callStatus(t, db, "c-queued"); after != before {
		t.Fatalf("events %d->%d, want no write", before, after)
	}
}

// TestCostForeignControlIDBeforeTerminalIsRefused checks that the binding still
// applies before termination: a cost naming another leg is refused and stored
// nowhere.
func TestCostForeignControlIDBeforeTerminalIsRefused(t *testing.T) {
	s, db := newWebhookStore(t)
	seedCall(t, db, "c-foreign", "active", "cc-real")
	f := newFixture(t, s)

	rec := f.post(t, "c-foreign", event(t, "c-foreign", "cost-foreign-1", "call.cost", "cc-OTHER", costPayload()))
	if rec.Code != http.StatusOK {
		t.Fatalf("foreign cost = %d, want 200 (permanent refusal)", rec.Code)
	}
	status, _, _ := providerCost(t, db, "c-foreign")
	if status != "" {
		t.Fatalf("foreign cost stored as %q", status)
	}
}
