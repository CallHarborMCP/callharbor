package storage

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// hangupAudit is the audit row a hangup writes. The storage test only checks
// that it commits with the decision.
func hangupAudit(callID string) AuditEvent {
	return AuditEvent{
		OccurredAt: testTime, ActorUserID: "alice", ActorRole: "user",
		Action: "call.hangup", ResourceType: "call", ResourceID: callID,
		Outcome: "success", DetailJSON: `{"outcome":"success"}`,
	}
}

func countAuditActions(t *testing.T, s *Store, action string) int {
	t.Helper()
	n, err := s.CountAuditEvents(context.Background(), action, "success")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRequestHangupQueuedFailsWithoutDial(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "q.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "c-q", "initiating", "")
	if _, err := s.db.ExecContext(ctx, `UPDATE calls SET status = 'queued' WHERE id = 'c-q'`); err != nil {
		t.Fatal(err)
	}

	d, err := s.RequestHangup(ctx, "c-q", hangupAudit("c-q"), mustParse(testTime))
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != HangupActionFailed || d.Status != "failed" {
		t.Fatalf("decision = %+v, want failed", d)
	}
	// The worker claim can no longer match the call, so it is never dialed.
	if err := s.ClaimQueuedCall(ctx, "c-q", mustParse(testTime)); err == nil {
		t.Fatal("ClaimQueuedCall succeeded after a hangup failed the call")
	}
	if got := countAuditActions(t, s, "call.hangup"); got != 1 {
		t.Fatalf("call.hangup audit rows = %d, want 1", got)
	}
}

func TestRequestHangupInFlightDialMovesToEndingWithoutLeg(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "d.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "c-d", "initiating", "")
	// seedLiveCall with no control id leaves the call queued. The worker claim
	// is what puts a dial in flight, so the test makes the same claim.
	if err := s.ClaimQueuedCall(ctx, "c-d", mustParse(testTime)); err != nil {
		t.Fatal(err)
	}

	d, err := s.RequestHangup(ctx, "c-d", hangupAudit("c-d"), mustParse(testTime))
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != HangupActionAwaitingDial || d.Status != "ending" {
		t.Fatalf("decision = %+v, want awaiting_dial in ending", d)
	}
	if d.Leg.ControlID != "" {
		t.Fatalf("leg control id = %q, want none while the dial is in flight", d.Leg.ControlID)
	}
}

func TestRequestHangupLiveCallSendsLegWithOwnersKeyFields(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "l.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "c-l", "active", "v3:live")

	d, err := s.RequestHangup(ctx, "c-l", hangupAudit("c-l"), mustParse(testTime))
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != HangupActionSendLeg || d.Status != "ending" {
		t.Fatalf("decision = %+v, want send_leg in ending", d)
	}
	if d.Leg.ControlID != "v3:live" || d.Leg.TelephonyOwnerUserID != "alice" {
		t.Fatalf("leg = %+v, want the control id and telephony owner", d.Leg)
	}
	if d.Leg.TelephonyProfileID != "alice-tel" {
		t.Fatalf("profile = %q, want alice-tel", d.Leg.TelephonyProfileID)
	}
}

// A repeated hangup on an ending call is a read: it changes nothing, but it
// still returns the leg so the caller can resend the stable command.
func TestRequestHangupRepeatOnEndingIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "r.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "c-r", "active", "v3:rep")

	if _, err := s.RequestHangup(ctx, "c-r", hangupAudit("c-r"), mustParse(testTime)); err != nil {
		t.Fatal(err)
	}
	d, err := s.RequestHangup(ctx, "c-r", hangupAudit("c-r"), mustParse(testTime))
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != HangupActionSendLeg || d.Status != "ending" {
		t.Fatalf("repeat decision = %+v, want send_leg in ending", d)
	}
	if got := countEventsOfKind(t, s, "c-r", "call.ending"); got != 1 {
		t.Fatalf("call.ending events = %d, want 1: a repeat must not append another", got)
	}
}

func TestRequestHangupTerminalCallIsNoOp(t *testing.T) {
	for _, status := range []string{"ended", "failed"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			s := openMigrated(t, filepath.Join(t.TempDir(), status+".db"))
			seedTenants(t, s)
			seedLiveCall(t, s, "c-t", status, "v3:term")

			d, err := s.RequestHangup(ctx, "c-t", hangupAudit("c-t"), mustParse(testTime))
			if err != nil {
				t.Fatal(err)
			}
			if d.Action != HangupActionNone || d.Status != status {
				t.Fatalf("decision = %+v, want none in %s", d, status)
			}
			if d.Leg.ControlID != "" {
				t.Fatalf("terminal decision carried a leg: %+v", d.Leg)
			}
		})
	}
}

func TestRequestHangupUnknownCallIsNotFound(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "n.db"))
	seedTenants(t, s)
	if _, err := s.RequestHangup(context.Background(), "missing", hangupAudit("missing"), mustParse(testTime)); err == nil {
		t.Fatal("RequestHangup on a missing call returned no error")
	}
}

// TestRequestHangupRacesClaimOnce runs a hangup and a worker claim on the same
// queued call, many times. The claim and the hangup are conditional on
// status = queued, so exactly one of them wins. If the claim wins, the call is
// initiating and the hangup moves it to ending, not failed. Either way the call
// is never claimed after it has failed.
func TestRequestHangupRacesClaimOnce(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		s := openMigrated(t, filepath.Join(t.TempDir(), "race.db"))
		seedTenants(t, s)
		id := "c-race"
		if err := s.InsertCall(ctx, newCallInsert(id, "alice", "alice-tel", "alice-ai", "k-race")); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		var claimErr error
		var decision HangupDecision
		var hangupErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			claimErr = s.ClaimQueuedCall(ctx, id, mustParse(testTime))
		}()
		go func() {
			defer wg.Done()
			<-start
			decision, hangupErr = s.RequestHangup(ctx, id, hangupAudit(id), mustParse(testTime))
		}()
		close(start)
		wg.Wait()

		if hangupErr != nil {
			t.Fatalf("iteration %d: hangup error: %v", i, hangupErr)
		}
		claimed := claimErr == nil
		if !claimed && claimErr != ErrCallNotClaimable {
			t.Fatalf("iteration %d: claim error = %v", i, claimErr)
		}
		switch {
		case claimed && decision.Action != HangupActionAwaitingDial:
			// The claim won, so the hangup found an initiating call and set ending
			// with no control id yet.
			t.Fatalf("iteration %d: claim won but decision = %+v, want awaiting_dial", i, decision)
		case !claimed && decision.Action != HangupActionFailed:
			t.Fatalf("iteration %d: hangup won but decision = %+v, want failed", i, decision)
		}
		// Only one of the two transitions wrote a state change.
		c, err := s.GetCallByIdempotencyKey(ctx, "alice", "k-race")
		if err != nil {
			t.Fatal(err)
		}
		if claimed && c.Status != "ending" || !claimed && c.Status != "failed" {
			t.Fatalf("iteration %d: status %q after claim=%v", i, c.Status, claimed)
		}
		_ = s.Close()
	}
}

// A dial that returns after a hangup records its control id on the ending
// call, and the call stays ending so the worker sends its one hangup.
func TestRecordControlIDOnEndingCallReturnsLegToHangUp(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "e.db"))
	seedTenants(t, s)
	if err := s.InsertCall(ctx, newCallInsert("c-e", "alice", "alice-tel", "alice-ai", "k-e")); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimQueuedCall(ctx, "c-e", mustParse(testTime)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestHangup(ctx, "c-e", hangupAudit("c-e"), mustParse(testTime)); err != nil {
		t.Fatal(err)
	}
	leg, status, err := s.RecordProviderCallControlID(ctx, "c-e", "alice", "v3:late", mustParse(testTime))
	if err != nil {
		t.Fatal(err)
	}
	if status != "ending" || leg.ControlID != "v3:late" || leg.TelephonyOwnerUserID != "alice" {
		t.Fatalf("leg = %+v status %q, want ending with the recorded control id", leg, status)
	}
}
