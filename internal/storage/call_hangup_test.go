package storage

import (
	"context"
	"path/filepath"
	"testing"
)

// seedLiveCall inserts a call owned by alice on alice-tel. With a control id it
// is claimed and dialed, the same path the worker takes. Then it is moved to
// status, which the schema allows only with ended_at set for terminal states.
func seedLiveCall(t *testing.T, s *Store, id, status, control string) {
	t.Helper()
	ctx := context.Background()
	if err := s.InsertCall(ctx, newCallInsert(id, "alice", "alice-tel", "alice-ai", "k-"+id)); err != nil {
		t.Fatal(err)
	}
	if control != "" {
		if err := s.ClaimQueuedCall(ctx, id, mustParse(testTime)); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordProviderCallControlID(ctx, id, "alice", control, mustParse(testTime)); err != nil {
			t.Fatal(err)
		}
	}
	if status == "initiating" {
		return
	}
	ended := ""
	if status == "ended" || status == "failed" {
		ended = mustParse(testTime).UTC().Format(storageTime)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE calls SET status = ?, ended_at = NULLIF(?, '') WHERE id = ?`,
		status, ended, id); err != nil {
		t.Fatal(err)
	}
}

// countEventsOfKind counts the call_events rows of one kind for a call.
func countEventsOfKind(t *testing.T, s *Store, callID, kind string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM call_events WHERE call_id = ? AND kind = ?`,
		callID, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBeginLocalHangupMovesLiveCallToEndingOnce(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "h.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "c-active", "active", "v3:one")

	leg, ok, err := s.BeginLocalHangup(ctx, "c-active", "max_duration", mustParse(testTime))
	if err != nil || !ok {
		t.Fatalf("BeginLocalHangup = ok %v, err %v; want a leg", ok, err)
	}
	if leg.ControlID != "v3:one" || leg.OwnerUserID != "alice" {
		t.Fatalf("leg = %+v, want the stored control id and owner", leg)
	}
	c, err := s.GetCallByIdempotencyKey(ctx, "alice", "k-c-active")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != "ending" {
		t.Fatalf("status = %q, want ending", c.Status)
	}

	// A second begin is a no-op: the call is already ending, so the caller
	// sends no second hangup from this path.
	if _, ok, err := s.BeginLocalHangup(ctx, "c-active", "max_duration", mustParse(testTime)); err != nil || ok {
		t.Fatalf("repeat BeginLocalHangup = ok %v, err %v; want no leg", ok, err)
	}
	if n := countEventsOfKind(t, s, "c-active", "call.ending"); n != 1 {
		t.Fatalf("call.ending events = %d, want exactly 1", n)
	}
}

func TestBeginLocalHangupLeavesTerminalCallsAlone(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "h2.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "c-ended", "ended", "v3:two")

	_, ok, err := s.BeginLocalHangup(ctx, "c-ended", "max_duration", mustParse(testTime))
	if err != nil || ok {
		t.Fatalf("BeginLocalHangup on ended = ok %v, err %v; want no leg", ok, err)
	}
	c, err := s.GetCallByIdempotencyKey(ctx, "alice", "k-c-ended")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != "ended" {
		t.Fatalf("terminal call moved to %q", c.Status)
	}
	if n := countEventsOfKind(t, s, "c-ended", "call.ending"); n != 0 {
		t.Fatalf("call.ending events on terminal call = %d, want 0", n)
	}
}

func TestBeginLocalHangupWithoutControlIDMovesButReturnsNoControl(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "h3.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "c-nocid", "active", "")

	leg, ok, err := s.BeginLocalHangup(ctx, "c-nocid", "media_failed", mustParse(testTime))
	if err != nil || !ok {
		t.Fatalf("BeginLocalHangup = ok %v, err %v", ok, err)
	}
	if leg.ControlID != "" {
		t.Fatalf("control id = %q, want empty so no hangup is sent", leg.ControlID)
	}
}

func TestBeginLocalHangupRequiresReason(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "h4.db"))
	if _, _, err := s.BeginLocalHangup(context.Background(), "c", "", mustParse(testTime)); err == nil {
		t.Fatal("empty reason accepted")
	}
}
