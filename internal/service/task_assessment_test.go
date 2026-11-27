package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/outcome"
)

const assessedAt = "2026-11-23T10:05:00.000Z"

// An unassessed call has task_outcome null and task_assessment null.
func TestUnassessedCallHasNullTaskAssessment(t *testing.T) {
	c := sampleCall("c1", "alice", ts)
	c.Status = "ended"
	v, err := toCallView(c)
	if err != nil {
		t.Fatal(err)
	}
	if v.TaskOutcome != nil || v.Details.TaskAssessment != nil {
		t.Fatalf("unassessed call: task_outcome=%v task_assessment=%+v", v.TaskOutcome, v.Details.TaskAssessment)
	}
}

// A recorded outcome maps its reason codes, source, time and evidence. The
// criterion is kept only when the evidence names one.
func TestAssessedCallMapsTaskAssessment(t *testing.T) {
	c := sampleCall("c1", "alice", ts)
	c.Status = "ended"
	c.Outcome = "success"
	c.OutcomeReasonCodes = []string{"confirmed"}
	c.OutcomeSource = "voice_session"
	c.OutcomeAssessedAt = assessedAt
	crit := 1
	c.OutcomeEvidence = []outcome.Evidence{
		{Kind: outcome.KindTranscriptTurn, Seq: 2, Criterion: &crit, Speaker: "callee", Fidelity: "model_derived"},
		{Kind: outcome.KindCallEvent, Seq: 1, Code: "answered", Fidelity: "exact"},
	}
	v, err := toCallView(c)
	if err != nil {
		t.Fatal(err)
	}
	ta := v.Details.TaskAssessment
	if ta == nil || v.TaskOutcome == nil || *v.TaskOutcome != "success" {
		t.Fatalf("view = %+v", v)
	}
	if ta.Source != "voice_session" || ta.AssessedAt != assessedAt || len(ta.ReasonCodes) != 1 || ta.ReasonCodes[0] != "confirmed" {
		t.Fatalf("assessment = %+v", ta)
	}
	if len(ta.Evidence) != 2 || ta.Evidence[0].Criterion == nil || *ta.Evidence[0].Criterion != 1 || ta.Evidence[1].Criterion != nil {
		t.Fatalf("evidence = %+v", ta.Evidence)
	}
	// Omitted optionals stay out of the JSON, not present as empty strings.
	b, err := json.Marshal(ta.Evidence[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"criterion"`, `"speaker"`} {
		if strings.Contains(string(b), key) {
			t.Fatalf("empty optional %s serialized: %s", key, b)
		}
	}
}

// Regression for the read surface: a recorded transcript evidence reference
// serialises with no quote key and no callee words, even when the stored
// evidence came from a verified quote. The text is read through the audited
// transcript endpoint, not from the call view.
func TestCallViewCarriesNoQuoteOrCalleeText(t *testing.T) {
	c := sampleCall("c1", "alice", ts)
	c.Status = "ended"
	c.Outcome = "success"
	c.OutcomeReasonCodes = []string{"confirmed"}
	c.OutcomeSource = "voice_session"
	c.OutcomeAssessedAt = assessedAt
	crit := 1
	const callee = "zebra-callee-words-7"
	c.OutcomeEvidence = []outcome.Evidence{
		{Kind: outcome.KindTranscriptTurn, Seq: 2, Criterion: &crit, Met: ptrTo(true), Speaker: "callee", Fidelity: "model_derived"},
	}
	v, err := toCallView(c)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"quote"`) || strings.Contains(string(b), callee) {
		t.Fatalf("call view carries quote or callee text: %s", b)
	}
}

func ptrTo[T any](v T) *T { return &v }

// A recorded outcome with no assessment timestamp is corrupt. Storage writes the
// four supporting fields together, so the read reports an error instead of
// silently showing no assessment.
func TestOutcomeWithoutTimestampIsAnError(t *testing.T) {
	c := sampleCall("c1", "alice", ts)
	c.Status = "ended"
	c.Outcome = "failed"
	if _, err := toCallView(c); err == nil {
		t.Fatal("toCallView accepted a recorded outcome without assessed_at")
	}
}

// The report's criteria and constraints are bounded by the same limit as the
// brief's success criteria and constraints (M2). This pins the two constants
// together, so a change to one cannot silently drift from the other.
func TestReportItemLimitMatchesBriefListLimit(t *testing.T) {
	if outcome.MaxReportItems != maxListItems {
		t.Fatalf("outcome.MaxReportItems = %d, brief maxListItems = %d; they must match", outcome.MaxReportItems, maxListItems)
	}
}
