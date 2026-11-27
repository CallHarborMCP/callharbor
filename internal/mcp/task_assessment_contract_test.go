package mcp

import (
	"encoding/json"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/outcome"
	"github.com/CallHarborMCP/callharbor/internal/service"
)

// A populated task_assessment from the real view validates against the served
// get_call and list_calls output schemas, which are the single contract for
// both tools.
func TestPopulatedTaskAssessmentValidatesAgainstMCPSchemas(t *testing.T) {
	embedded, err := loadToolSchemas()
	if err != nil {
		t.Fatal(err)
	}
	crit := 1
	view := service.CallView{
		CallID: "c1", TelephonyProfileID: "tp", AIProfileID: "ai", ToNumber: "+14155550111",
		TechnicalStatus: "ended", TaskOutcome: strPtr("success"),
		Details: service.CallDetails{
			MaxDurationSeconds: 300,
			Cost:               service.CostView{Status: "unavailable"},
			TaskAssessment: &service.TaskAssessment{
				ReasonCodes: []string{"confirmed"},
				Evidence: []service.TaskEvidence{
					{Kind: outcome.KindTranscriptTurn, Seq: 2, Criterion: &crit, Quote: "booked for friday", Speaker: "callee", Fidelity: "model_derived"},
					{Kind: outcome.KindCallEvent, Seq: 1, Code: "answered", Fidelity: "exact"},
				},
				Source:     "voice_session",
				AssessedAt: "2026-11-23T10:05:00.000Z",
			},
		},
		TranscriptAvailable: true,
		CreatedAt:           "2026-11-23T10:00:00.000Z", UpdatedAt: "2026-11-23T10:05:00.000Z",
		EndedAt: strPtr("2026-11-23T10:05:00.000Z"),
	}

	get := compileSchema(t, "get_call.output", embedded[ToolGetCall].OutputSchema)
	list := compileSchema(t, "list_calls.output", embedded[ToolListCalls].OutputSchema)

	one := mustDecode(t, view)
	if err := get.Validate(one); err != nil {
		t.Fatalf("get_call rejected populated task_assessment: %v", err)
	}
	page := mustDecode(t, service.CallsResult{Calls: []service.CallView{view}})
	if err := list.Validate(page); err != nil {
		t.Fatalf("list_calls rejected populated task_assessment: %v", err)
	}
}

// Evidence outside the schema bounds is refused: a criterion one above the
// shared limit (outcome.MaxReportItems) is a contract violation, not something a
// client should see.
func TestMCPSchemaRejectsOutOfContractEvidence(t *testing.T) {
	embedded, err := loadToolSchemas()
	if err != nil {
		t.Fatal(err)
	}
	crit := outcome.MaxReportItems + 1
	view := service.CallView{
		CallID: "c1", TelephonyProfileID: "tp", AIProfileID: "ai", ToNumber: "+14155550111",
		TechnicalStatus: "ended", TaskOutcome: strPtr("partial"),
		Details: service.CallDetails{MaxDurationSeconds: 300, Cost: service.CostView{Status: "unavailable"},
			TaskAssessment: &service.TaskAssessment{ReasonCodes: []string{"other"}, Source: "voice_session", AssessedAt: "2026-11-23T10:05:00.000Z",
				Evidence: []service.TaskEvidence{{Kind: outcome.KindTranscriptTurn, Seq: 2, Criterion: &crit}}}},
		CreatedAt: "2026-11-23T10:00:00.000Z", UpdatedAt: "2026-11-23T10:05:00.000Z",
	}
	get := compileSchema(t, "get_call.output", embedded[ToolGetCall].OutputSchema)
	if err := get.Validate(mustDecode(t, view)); err == nil {
		t.Fatal("criterion 17 accepted by get_call output")
	}
}

func mustDecode(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return decodeArgs(t, string(b))
}

func strPtr(s string) *string { return &s }
