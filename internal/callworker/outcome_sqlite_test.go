package callworker

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/outcome"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// sqliteNow is the worker clock for these tests. Calls end well before it, so
// they are past the outcome grace period and ready to evaluate.
var sqliteNow = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

// openOutcomeStore opens a real SQLite store with the production migrations and
// one owner with a telephony and an AI profile, written through the exported
// storage writers, so the calls' foreign keys are real.
func openOutcomeStore(t *testing.T) *storage.Store {
	t.Helper()
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "outcome.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	ts := sqliteNow.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	if err := s.CreateUser(ctx, storage.User{ID: "alice", Username: "alice", Role: "user", PasswordHash: "hash", Active: true, CreatedAt: ts, UpdatedAt: ts},
		storage.AuditEvent{OccurredAt: ts, ActorUserID: "alice", ActorRole: "user", TargetUserID: "alice", Action: "user.create", ResourceType: "user", ResourceID: "alice", Outcome: "success", DetailJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSecret(ctx, storage.Secret{ID: "alice-key", OwnerUserID: "alice", Name: "key", Purpose: "telnyx_api_key",
		Ciphertext: []byte{0}, Nonce: []byte{0}, KeyVersion: 1, CreatedAt: ts}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTelephonyProfile(ctx, storage.TelephonyProfileRecord{ID: "alice-tel", OwnerUserID: "alice", Name: "main",
		Provider: "telnyx", CallerNumber: "+14155550100", APISecretID: "alice-key", ConnectivityStatus: "unknown", CreatedAt: ts, UpdatedAt: ts}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAIProfile(ctx, storage.AIProfile{ID: "alice-ai", OwnerUserID: "alice", Name: "live", Protocol: storage.AIProtocol("live_ws"),
		EndpointURL: "wss://ai.example.test/live", Model: "m", Voice: "v", CreatedAt: ts, UpdatedAt: ts}); err != nil {
		t.Fatal(err)
	}
	return s
}

// insertFailedCall writes a call with the given brief and fails it before any
// dial, so it is terminal with ended_at set and no transcript. It is ready to
// evaluate once the grace period has passed.
func insertFailedCall(t *testing.T, s *storage.Store, id, brief string) {
	t.Helper()
	ctx := context.Background()
	if err := s.InsertCall(ctx, storage.CallInsert{
		ID: id, OwnerUserID: "alice", TelephonyProfileID: "alice-tel", AIProfileID: "alice-ai",
		IdempotencyKey: "k-" + id, ToNumber: "+14155550111", MaxDurationSeconds: 600,
		BriefJSON: brief, BriefSHA256: strings.Repeat("a", 64), BriefVersion: 1,
		CreatedAt: sqliteNow.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkCallFailed(ctx, id, "dial_rejected", sqliteNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// TestSQLitePoisonCallDoesNotBlockAnotherCall: a call whose stored brief cannot
// be decoded is recorded unknown with evaluation_input_invalid on the first
// tick, and a second ready call in the same batch is still evaluated. Nothing
// is retried forever, and the poison call is not listed again.
func TestSQLitePoisonCallDoesNotBlockAnotherCall(t *testing.T) {
	s := openOutcomeStore(t)
	// The database accepts any valid JSON brief, but this one has the wrong shape:
	// success_criteria is a string, not an array, so the evaluation decode fails.
	insertFailedCall(t, s, "c-poison", `{"objective":"x","success_criteria":"not-an-array"}`)
	insertFailedCall(t, s, "c-good", `{"objective":"x","success_criteria":["a"]}`)

	d := &fakeDialer{result: telnyx.DialResult{CallControlID: "cc"}}
	w, err := New(s, fixedPlanner{dialer: d}, Options{
		Enabled: true, BatchSize: 16,
		Now:    func() time.Time { return sqliteNow },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Tick(context.Background())

	poison := mustOutcome(t, s, "c-poison")
	if poison.Outcome != outcome.Unknown || !hasCode(poison.ReasonCodes, outcome.CodeEvaluationInputInvalid) {
		t.Fatalf("poison call = %s %v, want unknown with %s", poison.Outcome, poison.ReasonCodes, outcome.CodeEvaluationInputInvalid)
	}
	good := mustOutcome(t, s, "c-good")
	if good.Outcome == "" {
		t.Fatalf("good call was not evaluated: %+v", good)
	}

	ids, err := s.ListCallsAwaitingOutcome(context.Background(), sqliteNow, OutcomeGrace, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("calls still awaiting outcome after one tick: %v", ids)
	}
}

// mustOutcome reads back the recorded outcome through the public call read.
func mustOutcome(t *testing.T, s *storage.Store, id string) outcome.Result {
	t.Helper()
	c, err := s.GetCall(context.Background(), "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Outcome == "" {
		t.Fatalf("call %s has no recorded outcome", id)
	}
	return outcome.Result{Outcome: outcome.Outcome(c.Outcome), ReasonCodes: c.OutcomeReasonCodes}
}

func hasCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

// TestSQLiteConstraintsFromBriefReachTheEvaluator: the brief has two constraints.
// A success report that reports no constraints must be unknown with
// constraint_unreported. Before the worker wired ConstraintCount, the evaluator
// saw zero constraints and recorded success, so this test fails without the fix.
func TestSQLiteConstraintsFromBriefReachTheEvaluator(t *testing.T) {
	ctx := context.Background()
	s := openOutcomeStore(t)
	const id = "c-constraints"
	brief := `{"objective":"x","success_criteria":["book a table for two"],"constraints":["before seven","no bar seating"]}`
	insertFailedCall(t, s, id, brief)
	// Make the call an answered call with a finalized transcript that confirms the
	// criterion, so the only thing that can stop success is the constraint count.
	if err := s.RecordTranscript(ctx, id, "alice", []storage.TranscriptRevision{
		{Seq: 1, Speaker: storage.SpeakerCallee, Finality: storage.FinalityFinal,
			Text: "Yes, we can book a table for two at six.", Fidelity: "played", TimingSource: storage.TimingProvider},
	}, []storage.CallEvent{
		{Kind: storage.TranscriptStartedKind, Provider: "internal", PayloadJSON: `{"quality_flags":[]}`, OccurredAt: sqliteNow.Format(storage.TimeLayout)},
		{Kind: storage.AssessmentReportedKind, Provider: "realtime_ws",
			PayloadJSON: `{"outcome":"success","reason":"confirmed","criteria":[{"index":1,"met":true,"evidence_quote":"book a table for two at six"}],"constraints":[],"evidence_quotes":[]}`,
			OccurredAt:  sqliteNow.Format(storage.TimeLayout)},
		{Kind: storage.TranscriptFinalizedKind, Provider: "internal", PayloadJSON: `{"quality_flags":[]}`, OccurredAt: sqliteNow.Format(storage.TimeLayout)},
	}); err != nil {
		t.Fatal(err)
	}

	w, err := New(s, fixedPlanner{dialer: &fakeDialer{result: telnyx.DialResult{CallControlID: "cc"}}}, Options{
		Enabled: true, BatchSize: 16,
		Now:    func() time.Time { return sqliteNow },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Tick(ctx)

	got := mustOutcome(t, s, id)
	if got.Outcome != outcome.Unknown || !hasCode(got.ReasonCodes, outcome.CodeConstraintUnreported) {
		t.Fatalf("success with no constraints reported = %s %v, want unknown with %s",
			got.Outcome, got.ReasonCodes, outcome.CodeConstraintUnreported)
	}
}
