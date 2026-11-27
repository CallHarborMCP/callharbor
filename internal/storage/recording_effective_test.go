package storage

import (
	"context"
	"path/filepath"
	"testing"
)

// setOptIn turns the owner's recording opt-in on or off through the same
// columns the preference write uses, respecting the users CHECK that an opt-in
// carries an acknowledgement.
func setOptIn(t *testing.T, s *Store, userID string, on bool) {
	t.Helper()
	ack := "NULL"
	if on {
		ack = "'" + testTime + "'"
	}
	v := 0
	if on {
		v = 1
	}
	if _, err := s.db.ExecContext(context.Background(),
		"UPDATE users SET recording_opt_in = ?, recording_acknowledged_at = "+ack+" WHERE id = ?", v, userID); err != nil {
		t.Fatalf("set opt-in: %v", err)
	}
}

func TestInsertCallRecordEffectiveRequiresOwnerOptIn(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		optIn     bool
		requested bool
		want      bool
	}{
		{"opt-in off, record requested is accepted but not effective", false, true, false},
		{"opt-in on, record requested", true, true, true},
		{"opt-in on, record not requested", true, false, false},
		{"opt-in off, record not requested", false, false, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openMigrated(t, filepath.Join(t.TempDir(), "eff.db"))
			seedTenants(t, s)
			if tc.optIn {
				setOptIn(t, s, "alice", true)
			}
			in := newCallInsert("c-eff", "alice", "alice-tel", "alice-ai", "eff-key")
			in.RecordRequested = tc.requested
			if err := s.InsertCall(ctx, in); err != nil {
				t.Fatalf("InsertCall must accept the request (case %d): %v", i, err)
			}
			c, err := s.GetCall(ctx, "alice", "c-eff")
			if err != nil {
				t.Fatal(err)
			}
			if c.RecordRequested != tc.requested || c.RecordEffective != tc.want {
				t.Fatalf("requested=%v effective=%v, want requested=%v effective=%v",
					c.RecordRequested, c.RecordEffective, tc.requested, tc.want)
			}
		})
	}
}

func TestInsertCallIdempotentReplayComparesRecordRequested(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "replay.db"))
	seedTenants(t, s)
	setOptIn(t, s, "alice", true)

	first := newCallInsert("c-r", "alice", "alice-tel", "alice-ai", "replay-key")
	first.RecordRequested = true
	if err := s.InsertCall(ctx, first); err != nil {
		t.Fatal(err)
	}
	again := newCallInsert("c-r2", "alice", "alice-tel", "alice-ai", "replay-key")
	again.RecordRequested = false
	if err := s.InsertCall(ctx, again); err != ErrCallIdempotencyConflict {
		t.Fatalf("replay with different record flag = %v, want ErrCallIdempotencyConflict", err)
	}
}

func TestInsertCallForgedOverrideLeavesNoRecordingState(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "forge.db"))
	seedTenants(t, s)
	in := newCallInsert("c-forge", "alice", "alice-tel", "alice-ai", "forge-key")
	in.RecordRequested = true
	if err := s.InsertCall(ctx, in); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetCall(ctx, "alice", "c-forge")
	if err != nil {
		t.Fatal(err)
	}
	if c.RecordEffective {
		t.Fatal("record_effective set without owner opt-in")
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM recordings WHERE call_id = 'c-forge'"); n != 0 {
		t.Fatalf("recordings rows = %d, want 0", n)
	}
}
