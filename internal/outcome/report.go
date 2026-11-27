package outcome

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// ReportToolName is the function the voice session calls to report its own
// assessment. It is the only tool CallHarbor declares to the model.
const ReportToolName = "report_task_outcome"

// MaxReportBytes bounds one report's arguments. A larger report is treated as
// invalid and recorded as InvalidPayload, never parsed.
const MaxReportBytes = 16 << 10

// MaxReportItems bounds the criteria and constraints arrays of one report. It
// equals the brief's limit on success criteria and constraints (service
// maxListItems), so every brief item can be reported. A test pins the two.
const MaxReportItems = 20

// InvalidPayload is the stored payload of a report that failed validation.
const InvalidPayload = `{"invalid":true}`

// Report is one validated voice-session self-report, as stored.
type Report struct {
	Outcome     string             `json:"outcome"`
	Reason      string             `json:"reason"`
	Criteria    []ReportCriterion  `json:"criteria"`
	Constraints []ReportConstraint `json:"constraints"`
	// EvidenceQuotes are optional outcome-level quotes: callee words that
	// support the outcome without naming a criterion. They let a brief with no
	// criteria succeed on verified evidence.
	EvidenceQuotes []string `json:"evidence_quotes"`
}

// ReportCriterion says whether one success criterion was met, with the callee's
// exact words that show it.
type ReportCriterion struct {
	Index         int    `json:"index"`
	Met           bool   `json:"met"`
	EvidenceQuote string `json:"evidence_quote"`
}

// ReportConstraint says whether one brief constraint was respected.
type ReportConstraint struct {
	Index     int  `json:"index"`
	Respected bool `json:"respected"`
}

// ReportTool returns the function declaration for the session. It is the one
// definition of the tool; the realtime adapter sends it verbatim.
//
// The schema uses only type, properties, required, enum and items. Bounds and
// closed-object rules are enforced by ParseReport, not by the endpoint, so a
// tool the endpoint rejects for a keyword it does not support cannot break call
// setup. Whether the endpoint accepts this declaration is unverified (R19).
func ReportTool() map[string]any {
	index := map[string]any{"type": "integer"}
	return map[string]any{
		"type": "function",
		"name": ReportToolName,
		"description": "Call exactly once, when the outcome is final, before ending the call. " +
			"Report whether the task succeeded, partly succeeded or failed. For each success criterion, " +
			"cite only the callee's exact words, copied from what the callee said. Never invent a quote.",
		"parameters": map[string]any{
			"type":     "object",
			"required": []string{"outcome", "reason", "criteria", "constraints"},
			"properties": map[string]any{
				"outcome": map[string]any{"type": "string", "enum": []string{string(Success), string(Partial), string(Failed)}},
				"reason":  map[string]any{"type": "string", "enum": reportReasons()},
				"criteria": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type":     "object",
						"required": []string{"index", "met", "evidence_quote"},
						"properties": map[string]any{
							"index":          index,
							"met":            map[string]any{"type": "boolean"},
							"evidence_quote": map[string]any{"type": "string"},
						},
					},
				},
				"constraints": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type":     "object",
						"required": []string{"index", "respected"},
						"properties": map[string]any{
							"index":     index,
							"respected": map[string]any{"type": "boolean"},
						},
					},
				},
				"evidence_quotes": map[string]any{
					"type":        "array",
					"description": "Optional. The callee's exact words that support the outcome as a whole, not a single criterion.",
					"items":       map[string]any{"type": "string"},
				},
			},
		},
	}
}

func reportReasons() []string {
	return []string{ReasonConfirmed, ReasonAlternativeAccepted, ReasonPartiallyFulfilled, ReasonCalleeRefused,
		ReasonCalleeUnable, ReasonVoicemail, ReasonWrongParty, ReasonOther}
}

// errInvalidReport is the one error for every validation failure. Its message
// never names the content that failed, so a log line cannot leak it.
var errInvalidReport = errors.New("outcome: invalid task report")

// wireReport mirrors Report with pointers, so a missing field is distinct from
// a false or zero value.
type wireReport struct {
	Outcome        *string           `json:"outcome"`
	Reason         *string           `json:"reason"`
	Criteria       *[]wireCriterion  `json:"criteria"`
	Constraints    *[]wireConstraint `json:"constraints"`
	EvidenceQuotes *[]string         `json:"evidence_quotes"`
}

type wireCriterion struct {
	Index         *int    `json:"index"`
	Met           *bool   `json:"met"`
	EvidenceQuote *string `json:"evidence_quote"`
}

type wireConstraint struct {
	Index     *int  `json:"index"`
	Respected *bool `json:"respected"`
}

// ParseReport validates one report strictly: bounded size, no unknown or
// missing fields, closed enums, indexes in range and unique, and quotes within
// MaxQuoteRunes. Any failure returns errInvalidReport.
func ParseReport(b []byte) (Report, error) {
	if len(b) == 0 || len(b) > MaxReportBytes {
		return Report{}, errInvalidReport
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var w wireReport
	if err := dec.Decode(&w); err != nil {
		return Report{}, errInvalidReport
	}
	if _, err := dec.Token(); err != io.EOF {
		return Report{}, errInvalidReport
	}
	if w.Outcome == nil || w.Reason == nil || w.Criteria == nil || w.Constraints == nil {
		return Report{}, errInvalidReport
	}
	r := Report{Outcome: *w.Outcome, Reason: *w.Reason}
	switch Outcome(r.Outcome) {
	case Success, Partial, Failed:
	default:
		return Report{}, errInvalidReport
	}
	if !validReason(r.Reason) {
		return Report{}, errInvalidReport
	}
	if len(*w.Criteria) > MaxReportItems || len(*w.Constraints) > MaxReportItems {
		return Report{}, errInvalidReport
	}
	seen := map[int]bool{}
	for _, c := range *w.Criteria {
		if c.Index == nil || c.Met == nil || c.EvidenceQuote == nil {
			return Report{}, errInvalidReport
		}
		if *c.Index < 1 || *c.Index > MaxReportItems || seen[*c.Index] {
			return Report{}, errInvalidReport
		}
		seen[*c.Index] = true
		if utf8.RuneCountInString(*c.EvidenceQuote) > MaxQuoteRunes {
			return Report{}, errInvalidReport
		}
		r.Criteria = append(r.Criteria, ReportCriterion{Index: *c.Index, Met: *c.Met, EvidenceQuote: *c.EvidenceQuote})
	}
	seen = map[int]bool{}
	for _, c := range *w.Constraints {
		if c.Index == nil || c.Respected == nil {
			return Report{}, errInvalidReport
		}
		if *c.Index < 1 || *c.Index > MaxReportItems || seen[*c.Index] {
			return Report{}, errInvalidReport
		}
		seen[*c.Index] = true
		r.Constraints = append(r.Constraints, ReportConstraint{Index: *c.Index, Respected: *c.Respected})
	}
	if w.EvidenceQuotes != nil {
		if len(*w.EvidenceQuotes) > MaxReportItems {
			return Report{}, errInvalidReport
		}
		for _, q := range *w.EvidenceQuotes {
			if utf8.RuneCountInString(q) > MaxQuoteRunes {
				return Report{}, errInvalidReport
			}
		}
		r.EvidenceQuotes = append([]string{}, *w.EvidenceQuotes...)
	}
	if r.EvidenceQuotes == nil {
		r.EvidenceQuotes = []string{}
	}
	if r.Criteria == nil {
		r.Criteria = []ReportCriterion{}
	}
	if r.Constraints == nil {
		r.Constraints = []ReportConstraint{}
	}
	return r, nil
}

// Payload returns the canonical JSON stored for a validated report.
func (r Report) Payload() (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", errInvalidReport
	}
	return string(b), nil
}

// AssessmentFromReport turns a validated report into an Assessment. A met
// criterion's quote is resolved to the earliest final callee turn that contains
// it as whole words. A quote that matches no such turn keeps Seq 0, so the
// evaluator counts it as unverified. Unmet criteria make no claim.
func AssessmentFromReport(r Report, turns []Turn) Assessment {
	a := Assessment{Outcome: r.Outcome, Reason: r.Reason}
	for _, c := range r.Constraints {
		a.ConstraintsReported = append(a.ConstraintsReported, c.Index)
		if !c.Respected {
			a.ConstraintsViolated = true
		}
	}
	for _, c := range r.Criteria {
		// An unmet criterion keeps its quote, so a refusal can be shown as
		// evidence; the evaluator counts only met claims toward success.
		claim := Claim{Criterion: c.Index, Quote: c.EvidenceQuote, Met: c.Met}
		claim.Seq = resolveQuote(claim, Outcome(r.Outcome), turns)
		a.Claims = append(a.Claims, claim)
	}
	for _, q := range r.EvidenceQuotes {
		// Outcome-level quotes name no criterion. They count as met evidence.
		claim := Claim{Criterion: 0, Quote: q, Met: true}
		claim.Seq = resolveQuote(claim, Outcome(r.Outcome), turns)
		a.Claims = append(a.Claims, claim)
	}
	return a
}

// resolveQuote returns the earliest final callee turn seq that fully supports
// the claim, using the same rule as verification (supportsTurn, with negation
// checked under negationChecked). It returns 0 when no turn does, and the claim
// then stays unverified.
func resolveQuote(c Claim, o Outcome, turns []Turn) int {
	best := 0
	negate := negationChecked(c, string(o))
	for _, t := range turns {
		if t.Speaker != "callee" || t.Finality != "final" {
			continue
		}
		if supportsTurn(c.Quote, negate, t) && (best == 0 || t.Seq < best) {
			best = t.Seq
		}
	}
	return best
}

// AssessmentFromPayload reads one stored assessment.reported payload. It
// reports false for the invalid marker and for any payload that does not parse.
func AssessmentFromPayload(payload string, turns []Turn) (Assessment, bool) {
	if payload == InvalidPayload {
		return Assessment{}, false
	}
	r, err := ParseReport([]byte(payload))
	if err != nil {
		return Assessment{}, false
	}
	return AssessmentFromReport(r, turns), true
}
