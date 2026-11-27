package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// limitInsert is a valid call insert for owner, with a unique id and key.
func limitInsert(id, owner string, at time.Time, l CallLimits) CallInsert {
	c := newCallInsert(id, owner, owner+"-tel", owner+"-ai", "k-"+id)
	c.CreatedAt = at
	c.Limits = l
	return c
}

func TestMigration0009CreatesPolicyTableAndCostColumns(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "m9.db"))
	if v, _ := s.SchemaVersion(ctx); v < 9 {
		t.Fatalf("schema version = %d, want at least 9", v)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM pragma_table_info('calls') WHERE name IN ('cost_status', 'cost_amount_micros', 'cost_currency')"); n != 3 {
		t.Fatalf("cost columns on calls = %d, want 3", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'user_call_policies'"); n != 1 {
		t.Fatalf("user_call_policies table missing")
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM pragma_table_info('calls') WHERE name IN ('hangup_requested_at', 'hangup_attempts')"); n != 2 {
		t.Fatalf("reconciliation marker columns on calls = %d, want 2", n)
	}
}

func TestMigration0009RoundTripDropsReconciliationMarkers(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "m9marker.db"))
	seedTenants(t, s)
	if err := s.MigrateDown(ctx, 8); err != nil {
		t.Fatalf("rollback 0009: %v", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM pragma_table_info('calls') WHERE name IN ('hangup_requested_at', 'hangup_attempts')"); n != 0 {
		t.Fatalf("reconciliation marker columns after rollback = %d, want 0", n)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM pragma_table_info('calls') WHERE name IN ('hangup_requested_at', 'hangup_attempts')"); n != 2 {
		t.Fatalf("reconciliation marker columns after re-migrate = %d, want 2", n)
	}
}

func TestMigration0009RoundTripKeepsCalls(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "m9rt.db"))
	seedTenants(t, s)
	if err := s.MigrateDown(ctx, 8); err != nil {
		t.Fatalf("rollback 0009: %v", err)
	}
	if v, _ := s.SchemaVersion(ctx); v != 8 {
		t.Fatalf("schema after rollback = %d, want 8", v)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM calls WHERE id = 'alice-call'"); n != 1 {
		t.Fatalf("call lost on rollback")
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
}

func TestCostCheckRejectsUnknownStatus(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "cost.db"))
	seedTenants(t, s)
	expectExecError(t, s, `UPDATE calls SET cost_status = 'guessed' WHERE id = 'alice-call'`)
}

func TestUserCallPolicyRoundTripAndAudit(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "policy.db"))
	seedTenants(t, s)
	conc := int64(2)
	cost := int64(1_500_000)
	p := UserCallPolicy{
		UserID: "alice", MaxConcurrentCalls: &conc, MaxCostMicros: &cost,
		AllowPrefixes: []string{"+1415"}, DenyPrefixes: []string{"+1900"},
		UpdatedAt: testTime, UpdatedBy: "alice",
	}
	if err := s.UpsertUserCallPolicy(ctx, p, AuditEvent{OccurredAt: testTime, ActorUserID: "alice",
		ActorRole: "user", TargetUserID: "alice", ResourceType: "user_call_policy", ResourceID: "alice",
		Action: "user_call_policy.update", Outcome: "success", DetailJSON: "{}"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetUserCallPolicy(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxConcurrentCalls == nil || *got.MaxConcurrentCalls != 2 || got.MaxCallsPerHour != nil || got.MaxCostMicros == nil || *got.MaxCostMicros != cost {
		t.Fatalf("stored numeric overrides = %+v", got)
	}
	if len(got.AllowPrefixes) != 1 || got.AllowPrefixes[0] != "+1415" || len(got.DenyPrefixes) != 1 {
		t.Fatalf("stored prefixes = %v / %v", got.AllowPrefixes, got.DenyPrefixes)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM audit_events WHERE action = 'user_call_policy.update' AND target_user_id = 'alice'"); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}
	none, err := s.GetUserCallPolicy(ctx, "bob")
	if err != nil || none.MaxConcurrentCalls != nil || len(none.AllowPrefixes) != 0 {
		t.Fatalf("user without row = %+v, %v; want empty overrides", none, err)
	}
}

func TestOwnerConcurrencyLimitRefusesAtCapacity(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "limit.db"))
	seedTenants(t, s)
	// alice-call is already queued, so the owner has one non-terminal call.
	limits := CallLimits{OwnerMaxConcurrent: 2}
	at := mustParse(testTime)
	if err := s.InsertCall(ctx, limitInsert("a-2", "alice", at, limits)); err != nil {
		t.Fatalf("second call: %v", err)
	}
	err := s.InsertCall(ctx, limitInsert("a-3", "alice", at, limits))
	if !errors.Is(err, ErrOwnerConcurrencyLimit) {
		t.Fatalf("third call err = %v, want ErrOwnerConcurrencyLimit", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM calls WHERE id = 'a-3'"); n != 0 {
		t.Fatalf("refused call wrote %d rows", n)
	}
}

func TestTerminalCallsReleaseConcurrencyCapacity(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "release.db"))
	seedTenants(t, s)
	limits := CallLimits{OwnerMaxConcurrent: 1}
	at := mustParse(testTime)
	if err := s.InsertCall(ctx, limitInsert("a-2", "alice", at, limits)); !errors.Is(err, ErrOwnerConcurrencyLimit) {
		t.Fatalf("expected limit before ending: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE calls SET status = 'failed', ended_at = ? WHERE id = 'alice-call'`, testTime); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCall(ctx, limitInsert("a-2", "alice", at, limits)); err != nil {
		t.Fatalf("after the first call ended, a new call must fit: %v", err)
	}
}

func TestGlobalConcurrencyLimitCountsEveryOwner(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "global.db"))
	seedTenants(t, s)
	at := mustParse(testTime)
	limits := CallLimits{GlobalMaxConcurrent: 2}
	// alice-call counts as one. Bob's call is the second; a third is refused.
	if err := s.InsertCall(ctx, limitInsert("b-1", "bob", at, limits)); err != nil {
		t.Fatalf("bob's call: %v", err)
	}
	err := s.InsertCall(ctx, limitInsert("b-2", "bob", at, limits))
	if !errors.Is(err, ErrGlobalConcurrencyLimit) {
		t.Fatalf("err = %v, want ErrGlobalConcurrencyLimit", err)
	}
}

func TestHourlyRateLimitUsesTrailingWindow(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "rate.db"))
	seedTenants(t, s)
	limits := CallLimits{OwnerMaxPerHour: 2}
	now := mustParse("2026-11-27T10:00:00.000Z")
	// alice-call (created at testTime) is outside the window of 10:00 in
	// this test, so only the calls made here count.
	if err := s.InsertCall(ctx, limitInsert("r-1", "alice", now.Add(-30*time.Minute), limits)); err != nil {
		t.Fatalf("r-1: %v", err)
	}
	if err := s.InsertCall(ctx, limitInsert("r-2", "alice", now, limits)); err != nil {
		t.Fatalf("r-2: %v", err)
	}
	if err := s.InsertCall(ctx, limitInsert("r-3", "alice", now, limits)); !errors.Is(err, ErrOwnerRateLimit) {
		t.Fatalf("r-3 err = %v, want ErrOwnerRateLimit", err)
	}
	// Once r-1 is more than an hour old it no longer counts.
	later := now.Add(31 * time.Minute)
	if err := s.InsertCall(ctx, limitInsert("r-4", "alice", later, limits)); err != nil {
		t.Fatalf("r-4 after window: %v", err)
	}
}

// TestConcurrentInsertsNeverExceedTheLimit fires many parallel inserts at a
// limit of k. Exactly k must succeed: the count and the insert share one
// immediate transaction, so no two writers can both see a free slot.
func TestConcurrentInsertsNeverExceedTheLimit(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "race.db"))
	seedTenants(t, s)
	// End the seeded call so the owner starts at zero. Calls cannot be deleted
	// outside retention purge, so a terminal status is the way to free capacity.
	if _, err := s.db.ExecContext(ctx, `UPDATE calls SET status = 'failed', ended_at = ? WHERE id = 'alice-call'`, testTime); err != nil {
		t.Fatal(err)
	}
	const k, parallel = 3, 25
	at := mustParse(testTime)
	limits := CallLimits{OwnerMaxConcurrent: k, GlobalMaxConcurrent: 100}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
		refused  int
		other    []error
	)
	for i := range parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.InsertCall(ctx, limitInsert(fmt.Sprintf("p-%02d", i), "alice", at, limits))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, ErrOwnerConcurrencyLimit):
				refused++
			default:
				other = append(other, err)
			}
		}()
	}
	wg.Wait()
	if len(other) != 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if accepted != k || refused != parallel-k {
		t.Fatalf("accepted=%d refused=%d, want %d and %d", accepted, refused, k, parallel-k)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM calls WHERE owner_user_id = 'alice' AND status = 'queued'"); n != k {
		t.Fatalf("stored queued calls = %d, want %d", n, k)
	}
}

func TestInsertCallRecordsCostEstimate(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "estimate.db"))
	seedTenants(t, s)
	amount := int64(250_000)
	c := limitInsert("est-1", "alice", mustParse(testTime), CallLimits{})
	c.Cost = CallCost{Status: "estimated", AmountMicros: &amount, Currency: "USD"}
	if err := s.InsertCall(ctx, c); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM calls WHERE id = 'est-1' AND cost_status = 'estimated' AND cost_amount_micros = 250000 AND cost_currency = 'USD'"); n != 1 {
		t.Fatalf("estimate not stored")
	}
}
