package storage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/call"
	"github.com/CallHarborMCP/callharbor/internal/outcome"
)

// The callee's words never reach persisted task evidence. The
// evaluator may check a quote against the transcript, but the stored
// outcome_evidence holds references only. The quote text here is distinctive, so
// any leak into the stored JSON is visible.
func TestPersistedOutcomeEvidenceHasNoQuoteText(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "evidence-noquote.db"))
	seedTenants(t, s)
	o10Ringing(t, s, "alice-call")
	o10Answer(t, s, "alice-call")
	o10Hangup(t, s, "alice-call", call.ReasonHangup)

	// The callee says the marker, and the report quotes it. The evaluator verifies
	// the quote against the turn, so the marker reaches the evaluator. It must not
	// reach the persisted evidence.
	const marker = "zebra-quote-marker-41"
	in := outcome.Input{
		Protocol: "realtime_ws", Answered: true, CriteriaCount: 1,
		Transcript: outcome.Transcript{Started: true, Finalized: true, Turns: []outcome.Turn{
			{Seq: 2, Speaker: "callee", Finality: "final", Text: "Yes, " + marker + " is booked for you.", Fidelity: "played"},
		}},
		Assessments: []outcome.Assessment{{
			Outcome: "success", Reason: outcome.ReasonConfirmed,
			Claims: []outcome.Claim{{Criterion: 1, Seq: 2, Quote: marker + " is booked for you", Met: true}},
		}},
	}
	res := outcome.Evaluate(in)
	if res.Outcome != outcome.Success {
		t.Fatalf("setup: verified quote = %s %v, want success so the marker is evaluated", res.Outcome, res.ReasonCodes)
	}
	if ok, err := s.RecordTaskOutcome(ctx, "alice-call", res, o10Now); err != nil || !ok {
		t.Fatalf("record: wrote=%v err=%v", ok, err)
	}

	// The stored column is read directly, so the check is on what is persisted.
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT outcome_evidence FROM calls WHERE id = ?`, "alice-call").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "quote") {
		t.Fatalf("persisted outcome_evidence has a quote key: %s", raw)
	}
	if strings.Contains(raw, marker) {
		t.Fatalf("persisted outcome_evidence has quote text: %s", raw)
	}
}

// A call ended with reason media_failed sets MediaFailed. BeginLocalHangup
// writes the call.ending event, the same path callstream takes when media fails,
// and LoadOutcomeFacts reads it back.
func TestBeginLocalHangupMediaFailedSetsMediaFailed(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "media-failed.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "c-media", "active", "ctl-media")

	if _, ok, err := s.BeginLocalHangup(ctx, "c-media", mediaFailedReason, mustParse(testTime)); err != nil || !ok {
		t.Fatalf("BeginLocalHangup = ok %v, err %v", ok, err)
	}
	if n := countEventsOfKind(t, s, "c-media", "call.ending"); n != 1 {
		t.Fatalf("call.ending events = %d, want 1", n)
	}
	facts, err := s.LoadOutcomeFacts(ctx, "c-media")
	if err != nil {
		t.Fatal(err)
	}
	if !facts.MediaFailed {
		t.Fatalf("MediaFailed = false after a call.ending with reason %q", mediaFailedReason)
	}
}

// Control: a call ended for another reason does not set MediaFailed.
func TestBeginLocalHangupOtherReasonDoesNotSetMediaFailed(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "media-other.db"))
	seedTenants(t, s)
	seedLiveCall(t, s, "c-other", "active", "ctl-other")

	if _, ok, err := s.BeginLocalHangup(ctx, "c-other", "local_hangup", mustParse(testTime)); err != nil || !ok {
		t.Fatalf("BeginLocalHangup = ok %v, err %v", ok, err)
	}
	facts, err := s.LoadOutcomeFacts(ctx, "c-other")
	if err != nil {
		t.Fatal(err)
	}
	if facts.MediaFailed {
		t.Fatal("MediaFailed = true for a local_hangup call")
	}
}
