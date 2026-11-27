package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// recordCostTx runs RecordProviderCost in its own transaction, as the webhook
// receipt does.
func recordCostTx(t *testing.T, s *Store, callID string, cost ProviderCost) error {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordProviderCost(context.Background(), tx, callID, cost); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func micros(v int64) *int64 { return &v }

func TestRecordProviderCostStoresKnownAndIsIdempotentPerEvent(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "cost.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "call_cost", "ended", "v3:ctl-cost")

	cost := ProviderCost{Status: "success", AmountMicros: micros(10600), Currency: "USD", EventID: "call.cost|evt-1"}
	if err := recordCostTx(t, s, "call_cost", cost); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Redelivery of the same event writes nothing new and is not an error.
	if err := recordCostTx(t, s, "call_cost", cost); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	c, err := s.GetCall(context.Background(), "alice", "call_cost")
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider.Status != "success" || c.Provider.AmountMicros == nil || *c.Provider.AmountMicros != 10600 || c.Provider.Currency != "USD" {
		t.Fatalf("provider = %+v, want success 10600 USD", c.Provider)
	}
	// The status and the estimate are untouched.
	if c.Status != "ended" {
		t.Fatalf("status = %q, want ended", c.Status)
	}
	if c.Estimate.Status != "" {
		t.Fatalf("estimate changed to %+v", c.Estimate)
	}
}

func TestRecordProviderCostRefusesADifferentSecondEvent(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "cost2.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "call_cost2", "ended", "v3:ctl-cost2")

	first := ProviderCost{Status: "success", AmountMicros: micros(100), Currency: "USD", EventID: "call.cost|a"}
	if err := recordCostTx(t, s, "call_cost2", first); err != nil {
		t.Fatal(err)
	}
	second := ProviderCost{Status: "success", AmountMicros: micros(999), Currency: "USD", EventID: "call.cost|b"}
	if err := recordCostTx(t, s, "call_cost2", second); !errors.Is(err, ErrCostAlreadyRecorded) {
		t.Fatalf("second event err = %v, want ErrCostAlreadyRecorded", err)
	}
	c, err := s.GetCall(context.Background(), "alice", "call_cost2")
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider.AmountMicros == nil || *c.Provider.AmountMicros != 100 {
		t.Fatalf("first cost overwritten: %+v", c.Provider)
	}
}

func TestRecordProviderCostErrorStoresNoAmount(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "cost3.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "call_cost3", "failed", "v3:ctl-cost3")

	if err := recordCostTx(t, s, "call_cost3", ProviderCost{Status: "error", EventID: "call.cost|e"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetCall(context.Background(), "alice", "call_cost3")
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider.Status != "error" || c.Provider.AmountMicros != nil || c.Provider.Currency != "" {
		t.Fatalf("provider = %+v, want error with no amount", c.Provider)
	}
}

func TestRecordProviderCostRejectsBadStatusAndUnknownCall(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "cost4.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "call_cost4", "ended", "v3:ctl-cost4")

	if err := recordCostTx(t, s, "call_cost4", ProviderCost{Status: "pending", EventID: "x"}); err == nil {
		t.Fatal("status pending accepted, want refusal")
	}
	if err := recordCostTx(t, s, "call_missing", ProviderCost{Status: "success", EventID: "x"}); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("unknown call err = %v, want ErrCallNotFound", err)
	}
}
