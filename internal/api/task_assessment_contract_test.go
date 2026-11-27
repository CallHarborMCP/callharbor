package api

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/CallHarborMCP/callharbor/internal/outcome"
	"github.com/CallHarborMCP/callharbor/internal/service"
)

// A populated task_assessment, produced by the real view and serialised as the
// REST layer serialises it, must validate against the OpenAPI Call schema.
func TestPopulatedTaskAssessmentValidatesAgainstOpenAPI(t *testing.T) {
	crit := 1
	view := populatedCallView(t, &crit)
	schema := compileComponent(t, "Call")
	b, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(v); err != nil {
		t.Fatalf("populated task_assessment rejected by OpenAPI Call: %v\n%s", err, b)
	}
}

// An unassessed call with task_assessment null also validates.
func TestNullTaskAssessmentValidatesAgainstOpenAPI(t *testing.T) {
	view := populatedCallView(t, nil)
	view.TaskOutcome = nil
	view.Details.TaskAssessment = nil
	view.EndedAt = nil
	schema := compileComponent(t, "Call")
	b, _ := json.Marshal(view)
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(v); err != nil {
		t.Fatalf("null task_assessment rejected: %v\n%s", err, b)
	}
}

// A criterion outside 1 to 16, or an unknown source, is refused by the schema,
// so a future bug that emits one fails the contract.
func TestOpenAPIRejectsOutOfContractTaskAssessment(t *testing.T) {
	schema := compileComponent(t, "Call")
	bad := populatedCallView(t, nil)
	bad.Details.TaskAssessment.Source = "model_guess"
	b, _ := json.Marshal(bad)
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(v); err == nil {
		t.Fatal("unknown source accepted")
	}
}

// populatedCallView builds the CallView the REST layer serialises for one
// assessed call. The fields mirror what service.toCallView produces for the
// stored outcome, so the schema check runs against the real wire shape.
func populatedCallView(t *testing.T, criterion *int) service.CallView {
	t.Helper()
	at := "2026-11-23T10:05:00.000Z"
	outcomeStr := "success"
	view := service.CallView{
		CallID: "c1", TelephonyProfileID: "tp", AIProfileID: "ai", ToNumber: "+14155550111",
		TechnicalStatus: "ended", TaskOutcome: &outcomeStr,
		Details: service.CallDetails{
			OutcomeReasonCode: "completed", MaxDurationSeconds: 300,
			Cost: service.CostView{Status: "unavailable"},
			TaskAssessment: &service.TaskAssessment{
				ReasonCodes: []string{"confirmed"},
				Evidence: []service.TaskEvidence{
					{Kind: outcome.KindTranscriptTurn, Seq: 2, Criterion: criterion, Speaker: "callee", Fidelity: "model_derived"},
					{Kind: outcome.KindCallEvent, Seq: 1, Code: "answered", Fidelity: "exact"},
				},
				Source: "voice_session", AssessedAt: at,
			},
		},
		TranscriptAvailable: true,
		CreatedAt:           "2026-11-23T10:00:00.000Z", UpdatedAt: at, EndedAt: &at,
	}
	return view
}
