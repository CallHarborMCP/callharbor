package outcome

import (
	"fmt"
	"strings"
	"testing"
)

const validReport = `{"outcome":"success","reason":"confirmed","criteria":[{"index":1,"met":true,"evidence_quote":"Booked for Friday"}],"constraints":[{"index":1,"respected":true}]}`

var reportTurns = []Turn{
	{Seq: 2, Speaker: "callee", Finality: "final", Text: "Sure, booked for Friday at noon.", Fidelity: "model_derived"},
}

// Stored payloads are re-validated on read. A valid one becomes an assessment
// whose quote resolves to the callee turn that says it.
func TestAssessmentFromPayloadValidResolvesQuote(t *testing.T) {
	a, ok := AssessmentFromPayload(validReport, reportTurns)
	if !ok {
		t.Fatal("valid payload rejected")
	}
	if a.Outcome != "success" || a.Reason != ReasonConfirmed || a.ConstraintsViolated {
		t.Fatalf("assessment = %+v", a)
	}
	if len(a.Claims) != 1 || a.Claims[0].Criterion != 1 || a.Claims[0].Seq != 2 {
		t.Fatalf("claims = %+v, want criterion 1 resolved to seq 2", a.Claims)
	}
}

func TestAssessmentFromPayloadRejectsInvalidPayloads(t *testing.T) {
	long := strings.Repeat("a", MaxQuoteRunes+1)
	cases := map[string]string{
		"invalid marker":   InvalidPayload,
		"empty":            ``,
		"bad outcome enum": `{"outcome":"maybe","reason":"confirmed","criteria":[],"constraints":[]}`,
		"bad reason enum":  `{"outcome":"success","reason":"guessed","criteria":[],"constraints":[]}`,
		"index above max":  fmt.Sprintf(`{"outcome":"success","reason":"confirmed","criteria":[{"index":%d,"met":true,"evidence_quote":"x"}],"constraints":[]}`, MaxReportItems+1),
		"index zero":       `{"outcome":"success","reason":"confirmed","criteria":[{"index":0,"met":true,"evidence_quote":"x"}],"constraints":[]}`,
		"oversized quote":  `{"outcome":"success","reason":"confirmed","criteria":[{"index":1,"met":true,"evidence_quote":"` + long + `"}],"constraints":[]}`,
		"unknown field":    `{"outcome":"success","reason":"confirmed","criteria":[],"constraints":[],"extra":1}`,
		"missing field":    `{"outcome":"success","reason":"confirmed","criteria":[]}`,
		"not json":         `{"outcome":`,
	}
	for name, payload := range cases {
		if _, ok := AssessmentFromPayload(payload, reportTurns); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}
