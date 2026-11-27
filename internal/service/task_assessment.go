package service

import (
	"fmt"

	"github.com/CallHarborMCP/callharbor/internal/outcome"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// TaskAssessment is the recorded task outcome's reasoning. It is present only
// once an outcome is recorded; task_outcome itself stays the enum.
type TaskAssessment struct {
	ReasonCodes []string       `json:"reason_codes"`
	Evidence    []TaskEvidence `json:"evidence"`
	Source      string         `json:"source"`
	AssessedAt  string         `json:"assessed_at"`
}

// TaskEvidence is one reference that supports a task outcome. It carries no
// callee words: read the text at Seq through the audited transcript endpoint.
// Empty optional fields are omitted; criterion is omitted when the evidence
// supports no particular success criterion.
type TaskEvidence struct {
	Kind      string `json:"kind"`
	Seq       int    `json:"seq"`
	Criterion *int   `json:"criterion,omitempty"`
	// Met is set only on transcript evidence that names a criterion. It is
	// omitted for call_event evidence and outcome-level quotes.
	Met      *bool  `json:"met,omitempty"`
	Speaker  string `json:"speaker,omitempty"`
	Fidelity string `json:"fidelity,omitempty"`
	Code     string `json:"code,omitempty"`
}

// taskAssessmentOf maps a stored outcome to its public assessment. It is nil
// while the call has no recorded outcome. Storage writes outcome and assessed_at
// together, so a recorded outcome with no timestamp is corrupt: it is an error,
// not a silent null.
func taskAssessmentOf(c storage.Call) (*TaskAssessment, error) {
	if c.Outcome == "" {
		return nil, nil
	}
	if c.OutcomeAssessedAt == "" {
		return nil, fmt.Errorf("service: call %s has a task outcome without assessed_at", c.ID)
	}
	assessed, err := formatTimestamp(c.OutcomeAssessedAt)
	if err != nil {
		return nil, err
	}
	a := &TaskAssessment{
		ReasonCodes: append([]string{}, c.OutcomeReasonCodes...),
		Evidence:    make([]TaskEvidence, 0, len(c.OutcomeEvidence)),
		Source:      c.OutcomeSource,
		AssessedAt:  assessed,
	}
	for _, e := range c.OutcomeEvidence {
		a.Evidence = append(a.Evidence, taskEvidenceOf(e))
	}
	return a, nil
}

func taskEvidenceOf(e outcome.Evidence) TaskEvidence {
	return TaskEvidence{
		Kind: e.Kind, Seq: e.Seq, Criterion: e.Criterion, Met: e.Met,
		Speaker: e.Speaker, Fidelity: e.Fidelity, Code: e.Code,
	}
}
