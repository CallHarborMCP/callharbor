package storage

import (
	"context"
	"database/sql"
	"fmt"
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

// A grantee's call is placed by bob on alice's profile. Its leg must carry the
// profile owner, alice, so a hangup resolves alice's key. The call owner alone
// is the wrong key holder (R1).
func TestLiveLegCarriesProfileOwnerForGranteeCall(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "g.db"))
	seedTenants(t, s)
	// The call insert requires a grant from the profile owner to the caller.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO telephony_profile_grants
		(profile_id, grantee_user_id, granted_by_user_id, created_at) VALUES ('alice-tel', 'bob', 'alice', ?)`,
		testTime); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCall(ctx, newCallInsert("bob-on-alice", "bob", "alice-tel", "bob-ai", "k-grantee")); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimQueuedCall(ctx, "bob-on-alice", mustParse(testTime)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordProviderCallControlID(ctx, "bob-on-alice", "bob", "v3:grantee", mustParse(testTime)); err != nil {
		t.Fatal(err)
	}

	legs, err := s.ListLiveCallLegs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(legs) != 1 {
		t.Fatalf("live legs = %d, want 1", len(legs))
	}
	if legs[0].OwnerUserID != "bob" || legs[0].TelephonyOwnerUserID != "alice" {
		t.Fatalf("leg owners = call %q, profile %q; want bob, alice", legs[0].OwnerUserID, legs[0].TelephonyOwnerUserID)
	}

	// The local hangup path reads the same owners inside its transaction.
	leg, ok, err := s.BeginLocalHangup(ctx, "bob-on-alice", "max_duration", mustParse(testTime))
	if err != nil || !ok {
		t.Fatalf("BeginLocalHangup = ok %v, err %v", ok, err)
	}
	if leg.TelephonyOwnerUserID != "alice" {
		t.Fatalf("local hangup profile owner = %q, want alice", leg.TelephonyOwnerUserID)
	}
}

// HangupSeen is false until a call.hangup is recorded for the call. It is true
// after RecordHangupSeen, which a hangup on an already-terminal call uses, so a
// later orphan hangup is not sent for the leg.
func TestHangupSeenMarkerSetByRecordHangupSeen(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "hs.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "c-hs", "ended", "cc-hs")

	// Each read uses its own receipt id. A repeated id is a redelivery, so its
	// process function never runs and the state read would be empty.
	reads := 0
	read := func() CallSignalState {
		t.Helper()
		reads++
		var st CallSignalState
		ran, err := s.RecordWebhookOnce(ctx, "telnyx", fmt.Sprintf("ev-read-%d", reads), mustParse(testTime), func(ctx context.Context, tx *sql.Tx) error {
			var err error
			st, err = ReadCallForSignal(ctx, tx, "c-hs")
			return err
		})
		if err != nil || ran {
			t.Fatalf("read %d: ran=%v err=%v", reads, ran, err)
		}
		return st
	}
	if read().HangupSeen {
		t.Fatal("HangupSeen = true before any hangup was recorded")
	}
	if _, err := s.RecordWebhookOnce(ctx, "telnyx", "ev-mark", mustParse(testTime), func(ctx context.Context, tx *sql.Tx) error {
		st, err := ReadCallForSignal(ctx, tx, "c-hs")
		if err != nil {
			return err
		}
		return RecordHangupSeen(ctx, tx, st, "ev-mark", mustParse(testTime))
	}); err != nil {
		t.Fatal(err)
	}
	got := read()
	if got.ID != "c-hs" || !got.HangupSeen {
		t.Fatalf("after RecordHangupSeen: id=%q HangupSeen=%v, want c-hs true", got.ID, got.HangupSeen)
	}
}
