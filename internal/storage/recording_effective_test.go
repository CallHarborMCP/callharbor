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

// An owner who opts out after accept has the call's effective flag cleared at
// media start: the stored flag is 0 and the view reports no recording. A second
// media read changes nothing.
func TestLoadCallForMediaClearsEffectiveAfterOptOut(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "media-opt.db"))
	seedTenants(t, s)
	setOptIn(t, s, "alice", true)
	in := newCallInsert("c-opt", "alice", "alice-tel", "alice-ai", "opt-key")
	in.RecordRequested = true
	if err := s.InsertCall(ctx, in); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, s, "SELECT record_effective FROM calls WHERE id = 'c-opt'"); n != 1 {
		t.Fatalf("stored record_effective = %d before opt-out, want 1", n)
	}
	setOptIn(t, s, "alice", false)

	m, err := s.LoadCallForMedia(ctx, "c-opt")
	if err != nil {
		t.Fatal(err)
	}
	if m.RecordEffective {
		t.Fatal("media view reports recording after the owner opted out")
	}
	if n := scalarInt(t, s, "SELECT record_effective FROM calls WHERE id = 'c-opt'"); n != 0 {
		t.Fatalf("stored record_effective = %d after media start, want 0", n)
	}
	if _, err := s.LoadCallForMedia(ctx, "c-opt"); err != nil {
		t.Fatalf("second media read: %v", err)
	}
	c, err := s.GetCall(ctx, "alice", "c-opt")
	if err != nil {
		t.Fatal(err)
	}
	if c.RecordEffective {
		t.Fatal("call view still reports record_effective after media start")
	}
}
