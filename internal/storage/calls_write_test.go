package storage

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newCallInsert(id, owner, tel, ai, key string) CallInsert {
	return CallInsert{
		ID: id, OwnerUserID: owner, TelephonyProfileID: tel, AIProfileID: ai,
		IdempotencyKey: key, ToNumber: "+14155550111", RecordRequested: false,
		MaxDurationSeconds: 600, BriefJSON: `{"objective":"x"}`, BriefSHA256: strings.Repeat("a", 64), BriefVersion: 1,
		CreatedAt: mustParse(testTime),
	}
}

func mustParse(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestInsertCallWritesRowBriefAndQueuedEvent(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "w.db"))
	seedTenants(t, s)

	if err := s.InsertCall(ctx, newCallInsert("c-1", "alice", "alice-tel", "alice-ai", "idem-1")); err != nil {
		t.Fatalf("InsertCall: %v", err)
	}
	c, err := s.GetCallByIdempotencyKey(ctx, "alice", "idem-1")
	if err != nil {
		t.Fatalf("GetCallByIdempotencyKey: %v", err)
	}
	if c.ID != "c-1" || c.Status != "queued" || c.MaxDurationSeconds != 600 {
		t.Fatalf("call = %+v", c)
	}
	b, err := s.GetCallBrief(ctx, "alice", "c-1")
	if err != nil || b.Version != 1 || b.ContentJSON != `{"objective":"x"}` {
		t.Fatalf("brief = %+v, %v", b, err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM call_events WHERE call_id = 'c-1' AND seq = 1 AND kind = 'call.queued'"); n != 1 {
		t.Fatalf("queued event rows = %d", n)
	}
}

func TestInsertCallIdempotencyKeyConflictIsOwnerScoped(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "w.db"))
	seedTenants(t, s)
	if err := s.InsertCall(ctx, newCallInsert("c-1", "alice", "alice-tel", "alice-ai", "same")); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCall(ctx, newCallInsert("c-2", "alice", "alice-tel", "alice-ai", "same")); !errors.Is(err, ErrCallIdempotencyConflict) {
		t.Fatalf("same owner duplicate key = %v, want conflict", err)
	}
	if _, err := s.GetCallByIdempotencyKey(ctx, "bob", "same"); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("foreign key lookup = %v, want not found", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM call_briefs WHERE call_id = 'c-2'"); n != 0 {
		t.Fatal("failed insert left a brief behind")
	}
}

func TestClaimQueuedCallIsConditionalAndOrdered(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "w.db"))
	seedTenants(t, s)
	if err := s.InsertCall(ctx, newCallInsert("c-1", "alice", "alice-tel", "alice-ai", "k")); err != nil {
		t.Fatal(err)
	}
	now := mustParse(testTime)
	if err := s.ClaimQueuedCall(ctx, "c-1", now); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := s.ClaimQueuedCall(ctx, "c-1", now); !errors.Is(err, ErrCallNotClaimable) {
		t.Fatalf("second claim = %v, want not claimable", err)
	}
	c, _ := s.GetCallByIdempotencyKey(ctx, "alice", "k")
	if c.Status != "initiating" {
		t.Fatalf("status = %s, want initiating", c.Status)
	}
}

func TestRecordControlIDAndSingleCallOwnership(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "w.db"))
	seedTenants(t, s)
	now := mustParse(testTime)
	if err := s.InsertCall(ctx, newCallInsert("c-1", "alice", "alice-tel", "alice-ai", "w-k1")); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCall(ctx, newCallInsert("c-2", "alice", "alice-tel", "alice-ai", "w-k2")); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimQueuedCall(ctx, "c-1", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordProviderCallControlID(ctx, "c-1", "alice", "v3:abc", now); err != nil {
		t.Fatalf("record control id: %v", err)
	}
	// Re-recording the same id is allowed; a different id is refused.
	if _, _, err := s.RecordProviderCallControlID(ctx, "c-1", "alice", "v3:abc", now); err != nil {
		t.Fatalf("idempotent re-record: %v", err)
	}
	if err := s.ClaimQueuedCall(ctx, "c-2", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordProviderCallControlID(ctx, "c-1", "alice", "v3:other", now); !errors.Is(err, ErrCallNotClaimable) {
		t.Fatalf("different id on c-1 = %v, want refused", err)
	}
	if _, _, err := s.RecordProviderCallControlID(ctx, "c-2", "alice", "v3:abc", now); err == nil {
		t.Fatal("control id reused across calls was accepted")
	}
	if _, _, err := s.RecordProviderCallControlID(ctx, "c-2", "bob", "v3:new", now); !errors.Is(err, ErrCallNotClaimable) {
		t.Fatalf("foreign owner record = %v, want refused", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM call_events WHERE call_id = 'c-1' AND kind = 'call.dial_accepted'"); n != 1 {
		t.Fatalf("dial_accepted events for c-1 = %d, want 1", n)
	}
}

func TestMarkCallFailedIsTerminalAndOnlyFromPreDialStates(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "w.db"))
	seedTenants(t, s)
	now := mustParse(testTime)
	if err := s.InsertCall(ctx, newCallInsert("c-1", "alice", "alice-tel", "alice-ai", "k")); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimQueuedCall(ctx, "c-1", now); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkCallFailed(ctx, "c-1", "dial_refused", now); err != nil {
		t.Fatalf("MarkCallFailed: %v", err)
	}
	c, _ := s.GetCallByIdempotencyKey(ctx, "alice", "k")
	if c.Status != "failed" || c.OutcomeReasonCode != "dial_refused" || c.EndedAt == "" {
		t.Fatalf("call = %+v", c)
	}
	if err := s.MarkCallFailed(ctx, "c-1", "again", now); !errors.Is(err, ErrCallNotClaimable) {
		t.Fatalf("second MarkCallFailed = %v, want refused", err)
	}
	if err := s.ClaimQueuedCall(ctx, "c-1", now); !errors.Is(err, ErrCallNotClaimable) {
		t.Fatalf("claim after failure = %v, want refused", err)
	}
}
