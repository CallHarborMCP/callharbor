package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// reconcileNow is the clock the reconciliation tests use. It is the testTime
// the seed helpers write, so ages are exact.
func reconcileNow(t *testing.T) time.Time {
	t.Helper()
	return mustParse(testTime)
}

func TestRecordHangupSentCountsAttemptsAndSetsMarker(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "reconcile.db"))
	seedTenants(t, s)
	ctx := context.Background()
	seedLiveCall(t, s, "c-mark", "ending", "v3:mark")
	at := reconcileNow(t).Add(5 * time.Second)

	for i := 0; i < 2; i++ {
		if err := s.RecordHangupSent(ctx, "c-mark", at); err != nil {
			t.Fatalf("RecordHangupSent: %v", err)
		}
	}
	var attempts int
	var marked string
	if err := s.db.QueryRow(`SELECT hangup_attempts, hangup_requested_at FROM calls WHERE id = 'c-mark'`).
		Scan(&attempts, &marked); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || marked != at.UTC().Format(storageTime) {
		t.Fatalf("attempts=%d marked=%q, want 2 and %q", attempts, marked, at.UTC().Format(storageTime))
	}
	if err := s.RecordHangupSent(ctx, "missing", at); err != ErrCallNotFound {
		t.Fatalf("missing call error = %v, want ErrCallNotFound", err)
	}
}

func TestListEndingForReconcileRespectsCutoffAndHangupSeen(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "reconcile.db"))
	seedTenants(t, s)
	ctx := context.Background()
	seedLiveCall(t, s, "c-old", "ending", "v3:old")
	seedLiveCall(t, s, "c-seen", "ending", "v3:seen")
	seedLiveCall(t, s, "c-active", "active", "v3:active")

	// c-old was last touched at testTime. A cutoff after it selects it.
	cutoff := reconcileNow(t).Add(time.Minute)
	got, err := s.ListEndingForReconcile(ctx, cutoff, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ending calls = %d, want c-old and c-seen", len(got))
	}

	// A hangup event marks c-seen as confirmed by Telnyx. It must drop out.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO call_events (call_id, owner_user_id, seq, kind, occurred_at, recorded_at, provider)
		VALUES ('c-seen', 'alice', 99, 'hangup', ?, ?, 'telnyx')`, reconcileNow(t).UTC().Format(storageTime), reconcileNow(t).UTC().Format(storageTime)); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListEndingForReconcile(ctx, cutoff, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "c-old" {
		t.Fatalf("ending calls = %+v, want only c-old", got)
	}

	// A cutoff at the call's own timestamp is not strictly older, so it is not
	// yet due.
	got, err = s.ListEndingForReconcile(ctx, reconcileNow(t), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ending calls at the boundary = %d, want none (strictly older than 60 s)", len(got))
	}
}

func TestListEndingForReconcileIsBoundedByLimit(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "reconcile.db"))
	seedTenants(t, s)
	ctx := context.Background()
	for _, id := range []string{"c-b1", "c-b2", "c-b3"} {
		seedLiveCall(t, s, id, "ending", "v3:"+id)
	}
	got, err := s.ListEndingForReconcile(ctx, reconcileNow(t).Add(time.Minute), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("batch = %d, want the limit of 2", len(got))
	}
}

func TestListOverdueCallsSelectsOnlyPastDeadline(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "reconcile.db"))
	seedTenants(t, s)
	ctx := context.Background()
	seedLiveCall(t, s, "c-ring", "ringing", "v3:ring")
	seedLiveCall(t, s, "c-init", "initiating", "v3:init")

	// Both rows are created and updated at testTime with max 600 s. An initiating
	// call is overdue after the slack alone, since its dial already returned. A
	// ringing call needs its full time limit plus the slack.
	slack := 90
	at := func(d time.Duration) time.Time { return reconcileNow(t).Add(d) }

	got, err := s.ListOverdueCalls(ctx, at(time.Duration(slack-1)*time.Second), slack, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("just before the slack overdue = %d, want none", len(got))
	}

	got, err = s.ListOverdueCalls(ctx, at(time.Duration(slack)*time.Second), slack, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "c-init" {
		t.Fatalf("after the slack overdue = %+v, want only the initiating call", got)
	}

	got, err = s.ListOverdueCalls(ctx, at(time.Duration(600+slack-1)*time.Second), slack, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("just before the ringing deadline = %d, want only the initiating call", len(got))
	}

	got, err = s.ListOverdueCalls(ctx, at(time.Duration(600+slack)*time.Second), slack, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("after the ringing deadline = %d, want the ringing and the initiating call", len(got))
	}
}

func TestListOrphanedTerminalStopsAtMaxAttempts(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "reconcile.db"))
	seedTenants(t, s)
	ctx := context.Background()
	seedLiveCall(t, s, "c-orph", "failed", "v3:orph")
	seedLiveCall(t, s, "c-done", "failed", "v3:done")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO call_events (call_id, owner_user_id, seq, kind, occurred_at, recorded_at, provider)
		VALUES ('c-done', 'alice', 99, 'call.reconciled', ?, ?, 'internal')`, reconcileNow(t).UTC().Format(storageTime), reconcileNow(t).UTC().Format(storageTime)); err != nil {
		t.Fatal(err)
	}

	cutoff := reconcileNow(t).Add(time.Minute)
	got, err := s.ListOrphanedTerminal(ctx, orphanBounds(t, cutoff, 3), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "c-orph" {
		t.Fatalf("orphans = %+v, want only c-orph (c-done is reconciled)", got)
	}

	for i := 0; i < 3; i++ {
		if err := s.RecordHangupSent(ctx, "c-orph", reconcileNow(t)); err != nil {
			t.Fatal(err)
		}
	}
	got, err = s.ListOrphanedTerminal(ctx, orphanBounds(t, cutoff, 3), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("orphans after max attempts = %d, want none", len(got))
	}
}

// orphanBounds builds the sweep bounds the worker uses by default: the Telnyx
// clamp of 30 to 14400 seconds, a 30 second ring timeout and a 60 second grace.
func orphanBounds(t *testing.T, lastHangupBefore time.Time, maxAttempts int) OrphanBounds {
	t.Helper()
	return OrphanBounds{
		LastHangupBefore: lastHangupBefore,
		Now:              reconcileNow(t),
		MaxAttempts:      maxAttempts,
		LimitMin:         30,
		LimitMax:         14400,
		ExpiryPadSeconds: 30 + 60,
	}
}

// TestListOrphanedTerminalSkipsLegsPastTheirDeadline covers should-fix 3. A
// terminal call with a control id whose leg deadline has passed (created_at plus
// the clamped time limit, ring timeout and grace) must not be hung up again. A
// call created recently, whose leg may still be up, is still listed.
func TestListOrphanedTerminalSkipsLegsPastTheirDeadline(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "reconcile.db"))
	seedTenants(t, s)
	ctx := context.Background()
	seedLiveCall(t, s, "c-old", "ended", "v3:old")
	seedLiveCall(t, s, "c-recent", "ended", "v3:recent")
	// The old call was created long before now, with a 600 second cap: its deadline
	// (created + 600 + 90) has passed. The recent one is still inside its window.
	old := reconcileNow(t).Add(-2 * time.Hour).UTC().Format(storageTime)
	recent := reconcileNow(t).Add(-100 * time.Second).UTC().Format(storageTime)
	if _, err := s.db.ExecContext(ctx, `UPDATE calls SET created_at = ?, max_duration_seconds = 600, hangup_attempts = 0 WHERE id = 'c-old'`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE calls SET created_at = ?, max_duration_seconds = 600, hangup_attempts = 0 WHERE id = 'c-recent'`, recent); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListOrphanedTerminal(ctx, orphanBounds(t, reconcileNow(t).Add(time.Minute), 3), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "c-recent" {
		t.Fatalf("orphans = %+v, want only c-recent (c-old is past its deadline)", got)
	}
}

func TestMarkReconcileEndedEndsOnceAndKeepsReason(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "reconcile.db"))
	seedTenants(t, s)
	ctx := context.Background()
	seedLiveCall(t, s, "c-rec", "ending", "v3:rec")
	if _, err := s.db.ExecContext(ctx, `UPDATE calls SET outcome_reason_code = 'hangup_requested' WHERE id = 'c-rec'`); err != nil {
		t.Fatal(err)
	}
	now := reconcileNow(t).Add(2 * time.Minute)

	ended, err := s.MarkReconcileEnded(ctx, "c-rec", "", now)
	if err != nil || !ended {
		t.Fatalf("first end = (%v, %v), want (true, nil)", ended, err)
	}
	var status, reason string
	if err := s.db.QueryRow(`SELECT status, outcome_reason_code FROM calls WHERE id = 'c-rec'`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "ended" || reason != "hangup_requested" {
		t.Fatalf("status=%q reason=%q, want ended with the existing reason", status, reason)
	}
	if n := countEventsOfKind(t, s, "c-rec", "call.reconciled"); n != 1 {
		t.Fatalf("call.reconciled events = %d, want 1", n)
	}

	ended, err = s.MarkReconcileEnded(ctx, "c-rec", "", now)
	if err != nil || ended {
		t.Fatalf("second end = (%v, %v), want (false, nil)", ended, err)
	}
	if n := countEventsOfKind(t, s, "c-rec", "call.reconciled"); n != 1 {
		t.Fatalf("call.reconciled events after repeat = %d, want still 1", n)
	}
}

func TestMarkReconcileEndedSetsExpiryReason(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "reconcile.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "c-exp", "ending", "v3:exp")
	ended, err := s.MarkReconcileEnded(context.Background(), "c-exp", "reconcile_expired", reconcileNow(t))
	if err != nil || !ended {
		t.Fatalf("end = (%v, %v), want (true, nil)", ended, err)
	}
	var reason string
	if err := s.db.QueryRow(`SELECT outcome_reason_code FROM calls WHERE id = 'c-exp'`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "reconcile_expired" {
		t.Fatalf("reason = %q, want reconcile_expired", reason)
	}
}

func TestMarkReconcileEndedSkipsCallThatIsNoLongerEnding(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "reconcile.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "c-live", "active", "v3:live")
	ended, err := s.MarkReconcileEnded(context.Background(), "c-live", "", reconcileNow(t))
	if err != nil || ended {
		t.Fatalf("end of an active call = (%v, %v), want (false, nil)", ended, err)
	}
}
