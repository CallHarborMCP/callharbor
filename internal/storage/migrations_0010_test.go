package storage

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/call"
	"github.com/CallHarborMCP/callharbor/internal/outcome"
)

// o10Now is the fixed clock for the outcome tests.
var o10Now = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

// o10Insert inserts a queued call for alice with the given brief JSON.
func o10Insert(t *testing.T, s *Store, id, brief string) {
	t.Helper()
	c := newCallInsert(id, "alice", "alice-tel", "alice-ai", "k-"+id)
	c.BriefJSON = brief
	if err := s.InsertCall(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

// o10Signal applies one Telnyx-derived signal to a call the way the webhook
// does: read the state, rebuild the state machine, apply the signal, and write
// the result with WriteCallSignal. kind and reason are the call.Kind and
// call.Reason values the webhook derives from the Telnyx event.
func o10Signal(t *testing.T, s *Store, id string, kind call.Kind, reason call.Reason, eventID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := ReadCallForSignal(ctx, tx, id)
	if err != nil {
		t.Fatal(err)
	}
	ob := call.NewOutbound(id)
	ob.State = call.State(cur.Status)
	ob.Reason = call.Reason(cur.ReasonCode)
	ob.ProviderCallControlID = cur.ProviderCallControlID
	changed, err := ob.Apply(call.Signal{Kind: kind, Reason: reason, EventID: eventID})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		if err := RecordHangupSeen(ctx, tx, cur, eventID, o10Now); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := WriteCallSignal(ctx, tx, cur, string(ob.State), string(kind),
		string(ob.Reason), eventID, "", o10Now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// o10Ringing moves a queued call to ringing, as a dial acceptance and the
// ringing signal do in production. It claims the call and records a control id.
func o10Ringing(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := context.Background()
	if err := s.ClaimQueuedCall(ctx, id, o10Now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RecordProviderCallControlID(ctx, id, "alice", "ctl-"+id, o10Now); err != nil {
		t.Fatal(err)
	}
	o10Signal(t, s, id, call.KindRinging, "", "ringing|"+id)
}

// o10Answer connects a ringing call, as the answered signal does in production.
func o10Answer(t *testing.T, s *Store, id string) {
	t.Helper()
	o10Signal(t, s, id, call.KindAnswered, "", "answered|"+id)
}

// o10Hangup ends a call with the state machine reason, as the hangup signal does.
func o10Hangup(t *testing.T, s *Store, id string, reason call.Reason) {
	t.Helper()
	o10Signal(t, s, id, call.KindHangup, reason, "hangup|"+id)
}

// o10EndedAt sets only the ended_at timestamp, so a readiness test can place a
// call at a specific age. The status and events still come from the production
// writers.
func o10EndedAt(t *testing.T, s *Store, id string, endedAt time.Time) {
	t.Helper()
	ts := endedAt.UTC().Format(storageTime)
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE calls SET ended_at = ?, updated_at = ? WHERE id = ?`, ts, ts, id); err != nil {
		t.Fatal(err)
	}
}

// o10Fail fails a queued call before any dial, through MarkCallFailed, the
// production writer the worker uses for dial refusals.
func o10Fail(t *testing.T, s *Store, id, reason string) {
	t.Helper()
	if err := s.MarkCallFailed(context.Background(), id, reason, o10Now); err != nil {
		t.Fatal(err)
	}
}

// o10Record stores a failed, technical outcome for a terminal call.
func o10Record(t *testing.T, s *Store, id string) bool {
	t.Helper()
	ok, err := s.RecordTaskOutcome(context.Background(), id, outcome.Result{
		Outcome: outcome.Failed, Source: outcome.SourceTechnical,
		ReasonCodes: []string{outcome.CodeNoAnswer}, Evidence: []outcome.Evidence{},
	}, o10Now)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// o10ExecErr runs raw SQL and returns its error, so a test can check the message.
func o10ExecErr(s *Store, stmt string, args ...any) error {
	_, err := s.db.ExecContext(context.Background(), stmt, args...)
	return err
}

func TestMigration0010AddsOutcomeColumnsAndIndex(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "m10.db"))
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM pragma_table_info('calls') WHERE name IN ('outcome_reason_codes', 'outcome_evidence', 'outcome_source', 'outcome_assessed_at')"); n != 4 {
		t.Fatalf("outcome columns on calls = %d, want 4", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'calls_outcome_pending_idx'"); n != 1 {
		t.Fatal("calls_outcome_pending_idx missing")
	}
}

func TestMigration0010RoundTripKeepsCallsAndTriggers(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "m10rt.db"))
	seedTenants(t, s)
	if err := s.MigrateDown(ctx, 9); err != nil {
		t.Fatalf("rollback 0010: %v", err)
	}
	if v, _ := s.SchemaVersion(ctx); v != 9 {
		t.Fatalf("schema after rollback = %d, want 9", v)
	}
	// outcome_reason_code is from 0001 and stays; only the four 0010 columns go.
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM pragma_table_info('calls') WHERE name IN ('outcome_reason_codes', 'outcome_evidence', 'outcome_source', 'outcome_assessed_at')"); n != 0 {
		t.Fatalf("0010 outcome columns after rollback = %d, want 0", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM pragma_table_info('calls') WHERE name = 'outcome_reason_code'"); n != 1 {
		t.Fatal("technical outcome_reason_code column must survive rollback")
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'calls_task_outcome%'"); n != 0 {
		t.Fatalf("outcome triggers after rollback = %d, want 0", n)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM calls WHERE id = 'alice-call'"); n != 1 {
		t.Fatal("call lost on rollback")
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'calls_guarded_delete'"); n != 1 {
		t.Fatal("calls_guarded_delete missing after rollback")
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM pragma_table_info('calls') WHERE name IN ('outcome_reason_codes', 'outcome_evidence', 'outcome_source', 'outcome_assessed_at')"); n != 4 {
		t.Fatalf("outcome columns after re-migrate = %d, want 4", n)
	}
}

func TestTaskOutcomeCheckRejectsBadJSONAndSource(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "m10check.db"))
	seedTenants(t, s)
	o10Fail(t, s, "alice-call", "dial_rejected")
	expectExecError(t, s, `UPDATE calls SET outcome = 'failed', outcome_reason_codes = '{', outcome_evidence = '[]', outcome_source = 'technical', outcome_assessed_at = 'x' WHERE id = 'alice-call'`)
	expectExecError(t, s, `UPDATE calls SET outcome = 'failed', outcome_reason_codes = '[]', outcome_evidence = '[]', outcome_source = 'guessed', outcome_assessed_at = 'x' WHERE id = 'alice-call'`)
}

func TestTaskOutcomeAllOrNoneTriggers(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "m10all.db"))
	seedTenants(t, s)
	o10Fail(t, s, "alice-call", "dial_rejected")

	if err := o10ExecErr(s, `UPDATE calls SET outcome = 'failed' WHERE id = 'alice-call'`); err == nil || !strings.Contains(err.Error(), "set together") {
		t.Fatalf("outcome alone: err = %v, want set-together abort", err)
	}
	if err := o10ExecErr(s, `UPDATE calls SET outcome_reason_codes = '[]' WHERE id = 'alice-call'`); err == nil || !strings.Contains(err.Error(), "set together") {
		t.Fatalf("reason codes alone: err = %v, want set-together abort", err)
	}
	err := o10ExecErr(s, `INSERT INTO calls (id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number,
		status, max_duration_seconds, created_at, updated_at, ended_at, outcome)
		VALUES ('bad-insert', 'alice', 'alice-tel', 'alice-ai', 'k-bad', '+14155550199', 'failed', 60, ?, ?, ?, 'failed')`,
		testTime, testTime, testTime)
	if err == nil || !strings.Contains(err.Error(), "set together") {
		t.Fatalf("insert with outcome alone: err = %v, want set-together abort", err)
	}
}

func TestTaskOutcomeWriteOnce(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "m10once.db"))
	seedTenants(t, s)
	o10Fail(t, s, "alice-call", "dial_rejected")
	if !o10Record(t, s, "alice-call") {
		t.Fatal("first outcome write reported no change")
	}
	if err := o10ExecErr(s, `UPDATE calls SET outcome = 'success' WHERE id = 'alice-call'`); err == nil || !strings.Contains(err.Error(), "write-once") {
		t.Fatalf("change outcome: err = %v, want write-once abort", err)
	}
	if err := o10ExecErr(s, `UPDATE calls SET outcome_source = 'voice_session' WHERE id = 'alice-call'`); err == nil || !strings.Contains(err.Error(), "write-once") {
		t.Fatalf("change source: err = %v, want write-once abort", err)
	}
	if err := o10ExecErr(s, `UPDATE calls SET outcome_assessed_at = '2030-01-01T00:00:00.000Z' WHERE id = 'alice-call'`); err == nil || !strings.Contains(err.Error(), "write-once") {
		t.Fatalf("change assessed_at: err = %v, want write-once abort", err)
	}
	if err := o10ExecErr(s, `UPDATE calls SET updated_at = updated_at WHERE id = 'alice-call'`); err != nil {
		t.Fatalf("unrelated update after outcome: %v", err)
	}
}

func TestRecordTaskOutcomeIsIdempotentAndTerminalOnly(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "m10idem.db"))
	seedTenants(t, s)

	// A queued call is not terminal, so nothing is written.
	if ok, err := s.RecordTaskOutcome(ctx, "alice-call", outcome.Result{Outcome: outcome.Failed, Source: outcome.SourceTechnical}, o10Now); err != nil || ok {
		t.Fatalf("queued call: wrote=%v err=%v, want no write", ok, err)
	}

	o10Ringing(t, s, "alice-call")
	o10Answer(t, s, "alice-call")
	o10Hangup(t, s, "alice-call", call.ReasonHangup)
	first := outcome.Result{
		Outcome:     outcome.Success,
		Source:      outcome.SourceVoiceSession,
		ReasonCodes: []string{outcome.ReasonConfirmed},
		Evidence:    []outcome.Evidence{{Kind: outcome.KindTranscriptTurn, Seq: 2, Quote: "booked", Speaker: "callee", Fidelity: "played"}},
	}
	if ok, err := s.RecordTaskOutcome(ctx, "alice-call", first, o10Now); err != nil || !ok {
		t.Fatalf("first write: wrote=%v err=%v", ok, err)
	}
	second := outcome.Result{Outcome: outcome.Failed, Source: outcome.SourceTechnical, ReasonCodes: []string{outcome.CodeBusy}}
	if ok, err := s.RecordTaskOutcome(ctx, "alice-call", second, o10Now.Add(time.Minute)); err != nil || ok {
		t.Fatalf("second write: wrote=%v err=%v, want no write", ok, err)
	}

	c, err := s.GetCall(ctx, "alice", "alice-call")
	if err != nil {
		t.Fatal(err)
	}
	if c.Outcome != "success" || c.OutcomeSource != "voice_session" || c.OutcomeAssessedAt == "" {
		t.Fatalf("stored outcome = %+v", c)
	}
	if !slices.Equal(c.OutcomeReasonCodes, []string{outcome.ReasonConfirmed}) {
		t.Fatalf("reason codes = %v", c.OutcomeReasonCodes)
	}
	if len(c.OutcomeEvidence) != 1 || c.OutcomeEvidence[0].Quote != "booked" || c.OutcomeEvidence[0].Seq != 2 {
		t.Fatalf("evidence = %+v", c.OutcomeEvidence)
	}
	if c.OutcomeReasonCode != "hangup" {
		t.Fatalf("technical reason code changed by outcome write: %q", c.OutcomeReasonCode)
	}
}

func TestListCallsAwaitingOutcomeReadiness(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "m10ready.db"))
	seedTenants(t, s)
	grace := 2 * time.Minute
	old := o10Now.Add(-time.Hour)

	// Answered, hung up, transcript finalized: ready regardless of age.
	o10Insert(t, s, "c-final", `{"objective":"x"}`)
	o10Ringing(t, s, "c-final")
	o10Answer(t, s, "c-final")
	o10Hangup(t, s, "c-final", call.ReasonHangup)
	o10EndedAt(t, s, "c-final", o10Now.Add(-time.Second))
	finalizeTranscript(t, s, "c-final")

	// Never rang or answered, failed at dial, ended just now: not ready until
	// grace has passed, so a late answered signal can still complete it (M7).
	o10Insert(t, s, "c-nomedia", `{"objective":"x"}`)
	o10Fail(t, s, "c-nomedia", "dial_rejected")
	o10EndedAt(t, s, "c-nomedia", o10Now.Add(-time.Second))

	// Never rang or answered, failed at dial, ended long ago: ready after grace.
	o10Insert(t, s, "c-nomedia-old", `{"objective":"x"}`)
	o10Fail(t, s, "c-nomedia-old", "dial_rejected")
	o10EndedAt(t, s, "c-nomedia-old", old)

	// Media started, not finalized, ended recently: not ready yet.
	o10Insert(t, s, "c-recent", `{"objective":"x"}`)
	o10Ringing(t, s, "c-recent")
	o10Answer(t, s, "c-recent")
	o10Hangup(t, s, "c-recent", call.ReasonHangup)
	o10EndedAt(t, s, "c-recent", o10Now.Add(-30*time.Second))
	startTranscript(t, s, "c-recent")

	// Media started, not finalized, ended exactly grace ago: ready.
	o10Insert(t, s, "c-boundary", `{"objective":"x"}`)
	o10Ringing(t, s, "c-boundary")
	o10Answer(t, s, "c-boundary")
	o10Hangup(t, s, "c-boundary", call.ReasonHangup)
	o10EndedAt(t, s, "c-boundary", o10Now.Add(-grace))
	startTranscript(t, s, "c-boundary")

	// Media started, not finalized, ended long ago: ready after grace.
	o10Insert(t, s, "c-stale", `{"objective":"x"}`)
	o10Ringing(t, s, "c-stale")
	o10Answer(t, s, "c-stale")
	o10Hangup(t, s, "c-stale", call.ReasonHangup)
	o10EndedAt(t, s, "c-stale", old)
	startTranscript(t, s, "c-stale")

	// Already assessed: never listed.
	o10Insert(t, s, "c-done", `{"objective":"x"}`)
	o10Fail(t, s, "c-done", "dial_rejected")
	o10EndedAt(t, s, "c-done", old)
	if !o10Record(t, s, "c-done") {
		t.Fatal("record c-done")
	}

	// Not terminal: never listed, even with a finalized transcript.
	o10Insert(t, s, "c-active", `{"objective":"x"}`)
	o10Ringing(t, s, "c-active")
	startTranscript(t, s, "c-active")
	finalizeTranscript(t, s, "c-active")

	got, err := s.ListCallsAwaitingOutcome(ctx, o10Now, grace, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"c-boundary", "c-final", "c-nomedia-old", "c-stale"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("awaiting outcome = %v, want %v", got, want)
	}
	limited, err := s.ListCallsAwaitingOutcome(ctx, o10Now, grace, 2)
	if err != nil || len(limited) != 2 {
		t.Fatalf("limit 2 returned %v, %v", limited, err)
	}
	if _, err := s.ListCallsAwaitingOutcome(ctx, o10Now, grace, 0); err == nil {
		t.Fatal("limit 0 must be refused")
	}
}

// startTranscript and finalizeTranscript write the transcript summary events
// with the production kinds (storage.TranscriptStartedKind and
// TranscriptFinalizedKind, written by internal/transcript/recorder.go).
func startTranscript(t *testing.T, s *Store, id string) {
	t.Helper()
	recordSummary(t, s, id, TranscriptStartedKind)
}

func finalizeTranscript(t *testing.T, s *Store, id string) {
	t.Helper()
	recordSummary(t, s, id, TranscriptFinalizedKind)
}

func recordSummary(t *testing.T, s *Store, id, kind string) {
	t.Helper()
	ctx := context.Background()
	if err := s.RecordTranscript(ctx, id, "alice", nil, []CallEvent{
		{Kind: kind, Provider: "internal", PayloadJSON: `{"quality_flags":[]}`, OccurredAt: o10Now.Format(storageTime)},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeCallRemovesOutcomeRow(t *testing.T) {
	s := openMigrated(t, filepath.Join(t.TempDir(), "m10purge.db"))
	seedTenants(t, s)
	o10Fail(t, s, "alice-call", "dial_rejected")
	if !o10Record(t, s, "alice-call") {
		t.Fatal("record outcome")
	}
	if err := s.PurgeCall(context.Background(), "alice-call", testTime); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n := scalarInt(t, s, "SELECT COUNT(*) FROM calls WHERE id = 'alice-call'"); n != 0 {
		t.Fatalf("call row remains after purge: %d", n)
	}
}

func TestRequestHangupStillReadsTaskOutcome(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "m10hang.db"))
	seedTenants(t, s)
	o10Fail(t, s, "alice-call", "dial_rejected")
	if !o10Record(t, s, "alice-call") {
		t.Fatal("record outcome")
	}
	d, err := s.RequestHangup(ctx, "alice-call", hangupAudit("alice-call"), o10Now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != HangupActionNone || d.Outcome != "failed" {
		t.Fatalf("decision = %+v, want none with outcome failed", d)
	}
}

// TestNoAnswerFactsAreFailedNoAnswer is the production-path check: a call that
// rings and hangs up with the no_answer reason, never answered, yields facts
// the evaluator turns into failed with no_answer.
func TestNoAnswerFactsAreFailedNoAnswer(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "m10noanswer.db"))
	seedTenants(t, s)
	o10Insert(t, s, "c-noans", `{"objective":"x","success_criteria":["a"]}`)
	o10Ringing(t, s, "c-noans")
	o10Hangup(t, s, "c-noans", call.ReasonNoAnswer)

	f, err := s.LoadOutcomeFacts(ctx, "c-noans")
	if err != nil {
		t.Fatal(err)
	}
	if f.Answered {
		t.Fatal("a call that never answered must not read as answered")
	}
	if f.ReasonCode != "no_answer" {
		t.Fatalf("effective reason = %q, want no_answer", f.ReasonCode)
	}
	if len(f.CallEvents) == 0 || f.CallEvents[len(f.CallEvents)-1].Kind != CallEventHangup {
		t.Fatalf("call events = %+v, want a trailing hangup", f.CallEvents)
	}
	got := outcome.Evaluate(outcome.Input{
		Protocol: f.Protocol, Answered: f.Answered, ReasonCode: f.ReasonCode,
		CriteriaCount: f.Criteria, CallEvents: callEventRefs(f.CallEvents),
	})
	if got.Outcome != outcome.Failed || !slices.Equal(got.ReasonCodes, []string{outcome.CodeNoAnswer}) {
		t.Fatalf("evaluated = %+v, want failed with no_answer", got)
	}
}

// TestPreAnswerTranscriptStartIsNotAnswered: media may start while the call is
// still ringing (endpoint.go accepts streams while initiating or ringing), so a
// transcript.started event alone does not prove the callee answered. A hangup
// with no_answer and no answered event must stay not answered.
func TestPreAnswerTranscriptStartIsNotAnswered(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "m10prestart.db"))
	seedTenants(t, s)
	o10Insert(t, s, "c-prestart", `{"objective":"x","success_criteria":["a"]}`)
	o10Ringing(t, s, "c-prestart")
	startTranscript(t, s, "c-prestart")
	o10Hangup(t, s, "c-prestart", call.ReasonNoAnswer)

	f, err := s.LoadOutcomeFacts(ctx, "c-prestart")
	if err != nil {
		t.Fatal(err)
	}
	if !f.TranscriptStarted {
		t.Fatal("fixture: transcript start was not recorded")
	}
	if f.Answered {
		t.Fatal("transcript start without an answered event must not read as answered")
	}
	got := outcome.Evaluate(outcome.Input{
		Protocol: f.Protocol, Answered: f.Answered, ReasonCode: f.ReasonCode,
		CriteriaCount: f.Criteria, CallEvents: callEventRefs(f.CallEvents),
	})
	if got.Outcome != outcome.Failed || !slices.Equal(got.ReasonCodes, []string{outcome.CodeNoAnswer}) {
		t.Fatalf("evaluated = %+v, want failed with no_answer", got)
	}
}

// callEventRefs adapts stored events to the evaluator's input, as the worker
// does. Storage keeps the raw kind; only the evaluator's codes are derived here.
func callEventRefs(events []CallEventFact) []outcome.CallEventRef {
	out := make([]outcome.CallEventRef, 0, len(events))
	for _, e := range events {
		out = append(out, outcome.CallEventRef{Seq: e.Seq, Code: e.ReasonCode, Fidelity: "exact"})
	}
	return out
}

func TestLoadOutcomeFactsGathersEvaluatorInput(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "m10facts.db"))
	seedTenants(t, s)
	o10Insert(t, s, "c-facts", `{"objective":"x","success_criteria":["a","b"],"constraints":["c"]}`)
	o10Ringing(t, s, "c-facts")
	o10Answer(t, s, "c-facts")
	o10Hangup(t, s, "c-facts", call.ReasonHangup)

	if err := s.RecordTranscript(ctx, "c-facts", "alice", []TranscriptRevision{
		{Seq: 1, Speaker: SpeakerCallee, Finality: FinalityFinal, Text: "Booked for Tuesday.", Fidelity: "played", TimingSource: TimingProvider},
		{Seq: 2, Speaker: SpeakerCallee, Finality: FinalityPartial, StartedMS: 5, Text: "Partial", Fidelity: "played"},
	}, []CallEvent{
		{Kind: TranscriptStartedKind, Provider: "internal", PayloadJSON: `{"quality_flags":[]}`},
		{Kind: AssessmentReportedKind, Provider: "realtime_ws", PayloadJSON: `{"outcome":"success","reason":"confirmed"}`},
		{Kind: TranscriptFinalizedKind, Provider: "internal", PayloadJSON: `{"quality_flags":[]}`},
	}); err != nil {
		t.Fatal(err)
	}

	f, err := s.LoadOutcomeFacts(ctx, "c-facts")
	if err != nil {
		t.Fatal(err)
	}
	if f.Protocol != "live_ws" || f.Criteria != 2 || f.Constraints != 1 {
		t.Fatalf("brief/protocol facts = %+v", f)
	}
	if !f.Answered || f.MediaFailed || !f.TranscriptStarted || !f.TranscriptFinalized {
		t.Fatalf("flags = %+v", f)
	}
	if f.ReasonCode != "hangup" {
		t.Fatalf("effective reason = %q, want hangup", f.ReasonCode)
	}
	if len(f.Turns) != 2 || f.Turns[0].Text != "Booked for Tuesday." || f.Turns[0].Speaker != SpeakerCallee {
		t.Fatalf("turns = %+v", f.Turns)
	}
	if len(f.Assessments) != 1 || !strings.Contains(f.Assessments[0].PayloadJSON, `"confirmed"`) {
		t.Fatalf("assessments = %+v", f.Assessments)
	}
	kinds := []string{}
	for _, e := range f.CallEvents {
		kinds = append(kinds, e.Kind)
	}
	if !slices.Equal(kinds, []string{CallEventAnswered, CallEventHangup}) {
		t.Fatalf("call event kinds = %v, want [answered hangup]", kinds)
	}
	if _, err := s.LoadOutcomeFacts(ctx, "missing"); err != ErrCallNotFound {
		t.Fatalf("missing call: err = %v, want ErrCallNotFound", err)
	}
}

// TestLateAnsweredWithinGraceIsNotFailed: a hangup with reason hangup arrives
// before the answered signal. The call is failed at first, but a late answered
// signal within the grace completes it as ended, because normal clearing is
// consistent with having been answered. It must not be judged failed early:
// nothing is ready until the grace has passed, and once evaluated it is not
// failed with a not-answered code.
func TestLateAnsweredWithinGraceIsNotFailed(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "m10late.db"))
	seedTenants(t, s)
	grace := 2 * time.Minute

	o10Insert(t, s, "c-late", `{"objective":"x"}`)
	o10Ringing(t, s, "c-late")
	o10Hangup(t, s, "c-late", call.ReasonHangup)
	o10EndedAt(t, s, "c-late", o10Now.Add(-10*time.Second))

	// Within grace, the call is not ready: a late answered signal may still come.
	if got, err := s.ListCallsAwaitingOutcome(ctx, o10Now, grace, 100); err != nil || slices.Contains(got, "c-late") {
		t.Fatalf("call listed inside grace: %v, %v", got, err)
	}

	// The late answered signal arrives within grace and completes the call.
	o10Answer(t, s, "c-late")

	// After grace it is ready, and the facts show it as answered.
	later := o10Now.Add(grace)
	if got, err := s.ListCallsAwaitingOutcome(ctx, later, grace, 100); err != nil || !slices.Contains(got, "c-late") {
		t.Fatalf("call not ready after grace: %v, %v", got, err)
	}
	facts, err := s.LoadOutcomeFacts(ctx, "c-late")
	if err != nil {
		t.Fatal(err)
	}
	if !facts.Answered {
		t.Fatalf("late answered call read as not answered: %+v", facts)
	}
	if facts.Status == "failed" {
		t.Fatalf("late answered call is still failed: status %q", facts.Status)
	}
}

// TestRecordTaskOutcomeKeepsUpdatedAt: the orphan sweep reads calls.updated_at
// as its retry anchor, so recording an outcome must leave it unchanged.
func TestRecordTaskOutcomeKeepsUpdatedAt(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "m10updated.db"))
	seedTenants(t, s)
	o10Fail(t, s, "alice-call", "dial_rejected")
	before := scalarString(t, s, "SELECT updated_at FROM calls WHERE id = 'alice-call'")

	ok, err := s.RecordTaskOutcome(ctx, "alice-call", outcome.Result{
		Outcome: outcome.Failed, Source: outcome.SourceTechnical,
		ReasonCodes: []string{outcome.CodeCallFailed}, Evidence: []outcome.Evidence{},
	}, o10Now.Add(time.Hour))
	if err != nil || !ok {
		t.Fatalf("record outcome: wrote=%v err=%v", ok, err)
	}
	if after := scalarString(t, s, "SELECT updated_at FROM calls WHERE id = 'alice-call'"); after != before {
		t.Fatalf("updated_at moved from %q to %q; recording an outcome must not change it", before, after)
	}
}

// TestEarlyMediaCalleeTurnIsNotAnsweredOnNoAnswer (N10): carrier early media can
// produce a final callee turn before a call that never connects is hung up with
// no_answer. With no answered event, the turn must not make the call answered,
// so the facts evaluate to failed with no_answer.
func TestEarlyMediaCalleeTurnIsNotAnsweredOnNoAnswer(t *testing.T) {
	ctx := context.Background()
	s := openMigrated(t, filepath.Join(t.TempDir(), "m10earlymedia.db"))
	seedTenants(t, s)
	o10Insert(t, s, "c-early", `{"objective":"x","success_criteria":["a"]}`)
	o10Ringing(t, s, "c-early")
	if err := s.RecordTranscript(ctx, "c-early", "alice", []TranscriptRevision{
		{Seq: 1, Speaker: SpeakerCallee, Finality: FinalityFinal, Text: "Hello, this is the restaurant.", Fidelity: "played", TimingSource: TimingProvider},
	}, []CallEvent{
		{Kind: TranscriptStartedKind, Provider: "internal", PayloadJSON: `{"quality_flags":[]}`, OccurredAt: o10Now.Format(storageTime)},
	}); err != nil {
		t.Fatal(err)
	}
	o10Hangup(t, s, "c-early", call.ReasonNoAnswer)

	f, err := s.LoadOutcomeFacts(ctx, "c-early")
	if err != nil {
		t.Fatal(err)
	}
	if f.Answered {
		t.Fatal("early-media callee turn with no answered event must not read as answered")
	}
	got := outcome.Evaluate(outcome.Input{
		Protocol: f.Protocol, Answered: f.Answered, ReasonCode: f.ReasonCode,
		CriteriaCount: f.Criteria, CallEvents: callEventRefs(f.CallEvents),
	})
	if got.Outcome != outcome.Failed || !slices.Equal(got.ReasonCodes, []string{outcome.CodeNoAnswer}) {
		t.Fatalf("evaluated = %+v, want failed with no_answer", got)
	}
}
