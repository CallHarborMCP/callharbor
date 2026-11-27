package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/call"
	"github.com/CallHarborMCP/callharbor/internal/outcome"
)

// AssessmentReportedKind is the call event that stores one voice-session
// self-report. Its payload is parsed by the caller, not by storage. The realtime
// recorder writes it (see transcript.Recorder.taskReportLocked).
const AssessmentReportedKind = "assessment.reported"

// Stored call-event kinds the task-outcome read depends on. Each one matches the
// production writer named beside it.
const (
	// CallEventAnswered is written by WriteCallSignal for the answered signal.
	CallEventAnswered = string(call.KindAnswered)
	// CallEventHangup is written by WriteCallSignal for the hangup signal, and by
	// RecordHangupSeen. WriteCallSignal's reason_code is the state machine's
	// reason after the signal, which for a hangup that closes an ending call is
	// the reason already on the call, not a Telnyx cause. RecordHangupSeen writes
	// no reason_code, so the reader falls back to outcome_reason_code.
	CallEventHangup = string(call.KindHangup)
	// CallEventFailed is written by MarkCallFailed and by RequestHangup, when a
	// queued call is failed before any dial. Its reason is a CallHarbor code, not
	// a Telnyx cause.
	CallEventFailed = "call.failed"
)

// mediaFailedReason is the reason_code of a call ended because its media run
// failed. callstream writes it (ReasonMediaFailed) through BeginLocalHangup.
// callstream imports storage, so the value is repeated here. The storage-level
// test TestBeginLocalHangupMediaFailedSetsMediaFailed pins it: it writes the
// call.ending event through BeginLocalHangup and checks that LoadOutcomeFacts
// reads MediaFailed. callstream's own tests pin its constant separately.
const mediaFailedReason = "media_failed"

// notConnectedNoAnswerOrBusy reports whether an effective reason says the callee
// never answered a call that rang: no answer, or busy. Such a call is not
// answered without an answered event, whatever early media said.
func notConnectedNoAnswerOrBusy(reason string) bool {
	return reason == string(call.ReasonNoAnswer) || reason == string(call.ReasonBusy)
}

// Bounds on what one evaluation reads. maxOutcomeTurns equals the transcript
// recorder's MaxTurns, the most turns one call can ever store, so a call that
// the recorder wrote is always evaluable. A call beyond a bound is not evaluated
// from a partial record. LoadOutcomeFacts returns ErrOutcomeTooLarge, and the
// worker records it as unknown instead of retrying forever.
const (
	maxOutcomeTurns       = 20000
	maxOutcomeAssessments = 100
)

// ErrOutcomeTooLarge reports a call whose transcript exceeds maxOutcomeTurns.
var ErrOutcomeTooLarge = errors.New("storage: call transcript exceeds the outcome evaluation bound")

// ErrOutcomeInputInvalid reports a call whose stored brief or facts cannot be
// read. It is permanent for that call, so it is recorded unknown, not retried.
var ErrOutcomeInputInvalid = errors.New("storage: outcome input is invalid")

// RecordTaskOutcome stores the evaluated task outcome of a terminal call. It is
// one conditional update that writes only while outcome is NULL and the call is
// ended or failed. It reports whether it wrote, so evaluating a call a second
// time changes nothing.
func (s *Store) RecordTaskOutcome(ctx context.Context, callID string, r outcome.Result, now time.Time) (bool, error) {
	if callID == "" {
		return false, errors.New("storage: record task outcome: empty call id")
	}
	switch r.Outcome {
	case outcome.Success, outcome.Partial, outcome.Failed, outcome.Unknown:
	default:
		return false, fmt.Errorf("storage: record task outcome: unknown outcome %q", r.Outcome)
	}
	switch r.Source {
	case outcome.SourceTechnical, outcome.SourceVoiceSession:
	default:
		return false, fmt.Errorf("storage: record task outcome: unknown source %q", r.Source)
	}
	codes := r.ReasonCodes
	if codes == nil {
		codes = []string{}
	}
	evidence := r.Evidence
	if evidence == nil {
		evidence = []outcome.Evidence{}
	}
	codesJSON, err := json.Marshal(codes)
	if err != nil {
		return false, fmt.Errorf("storage: encode outcome reason codes: %w", err)
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return false, fmt.Errorf("storage: encode outcome evidence: %w", err)
	}
	ts := now.UTC().Format(storageTime)
	// updated_at is deliberately not written. The orphan sweep uses it as the
	// retry anchor for a call's hangup, so recording an outcome must not move it.
	// No trigger depends on it either.
	res, err := s.db.ExecContext(ctx, `UPDATE calls SET outcome = ?, outcome_reason_codes = ?,
		outcome_evidence = ?, outcome_source = ?, outcome_assessed_at = ?
		WHERE id = ? AND outcome IS NULL AND status IN ('ended', 'failed')`,
		string(r.Outcome), string(codesJSON), string(evidenceJSON), string(r.Source), ts, callID)
	if err != nil {
		return false, fmt.Errorf("storage: record task outcome: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("storage: record task outcome rows: %w", err)
	}
	return n == 1, nil
}

// ListCallsAwaitingOutcome returns up to limit ids of terminal calls with no
// recorded task outcome that are ready to evaluate. A call is ready when its
// transcript is finalized, or when it ended at least grace before now. The grace
// applies to every call, answered or not, so a call that hung up before its
// answered signal arrived (a late answered signal can still complete it) is not
// judged as failed early. A transcript that never finalizes still gets an
// outcome once grace has passed. Oldest ended calls come first.
func (s *Store) ListCallsAwaitingOutcome(ctx context.Context, now time.Time, grace time.Duration, limit int) ([]string, error) {
	if limit < 1 {
		return nil, errors.New("storage: list calls awaiting outcome: limit must be positive")
	}
	cutoff := now.Add(-grace).UTC().Format(storageTime)
	rows, err := s.db.QueryContext(ctx, `SELECT c.id FROM calls c
		WHERE c.outcome IS NULL AND c.status IN ('ended', 'failed') AND (
			EXISTS (SELECT 1 FROM call_events e WHERE e.call_id = c.id AND e.kind = ?)
			OR c.ended_at IS NULL OR c.ended_at <= ?)
		ORDER BY c.ended_at, c.id LIMIT ?`,
		TranscriptFinalizedKind, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: list calls awaiting outcome: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("storage: scan call awaiting outcome: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: list calls awaiting outcome: %w", err)
	}
	return ids, nil
}

// OutcomeFacts is what the task-outcome evaluator reads for one call. Storage
// gathers it without interpreting it: the caller parses the assessment payloads
// and maps the rest to an outcome.Input.
type OutcomeFacts struct {
	CallID string
	Status string
	// ReasonCode is the effective technical reason: the latest non-empty hangup
	// event reason, else calls.outcome_reason_code.
	ReasonCode          string
	Protocol            string
	Answered            bool
	MediaFailed         bool
	Criteria            int
	Constraints         int
	TranscriptStarted   bool
	TranscriptFinalized bool
	// Turns are the latest revision of every turn, in seq order.
	Turns []TranscriptTurn
	// CallEvents are the answered, hangup and failure events, in seq order.
	CallEvents []CallEventFact
	// Assessments are the most recent stored self-reports, in seq order.
	Assessments []AssessmentFact
}

// CallEventFact is one technical call event that can be cited as evidence.
type CallEventFact struct {
	Seq        int
	Kind       string
	ReasonCode string
}

// AssessmentFact is one stored voice-session self-report, as written.
type AssessmentFact struct {
	Seq         int
	PayloadJSON string
}

// LoadOutcomeFacts reads everything the evaluator needs for one call. It is an
// internal worker read, so it does not filter by owner; the owner comes from
// the call row. The reads are not one snapshot. That is safe because transcript
// turns and events are append-only, and readiness waits for a finalized
// transcript before evaluation.
func (s *Store) LoadOutcomeFacts(ctx context.Context, callID string) (OutcomeFacts, error) {
	f := OutcomeFacts{CallID: callID}
	var owner string
	var reason sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT c.owner_user_id, c.status, c.outcome_reason_code, p.protocol
		FROM calls c JOIN ai_profiles p ON p.id = c.ai_profile_id AND p.owner_user_id = c.owner_user_id
		WHERE c.id = ?`, callID).Scan(&owner, &f.Status, &reason, &f.Protocol)
	if errors.Is(err, sql.ErrNoRows) {
		return OutcomeFacts{}, ErrCallNotFound
	}
	if err != nil {
		return OutcomeFacts{}, fmt.Errorf("storage: load outcome call: %w", err)
	}
	// The effective reason is the hangup event's reason, the state machine's
	// reason for the end. Where that is empty (the call was already ending when
	// the hangup arrived, or the event was recorded without one), it falls back
	// to outcome_reason_code, which the signal and local-hangup writers set.
	f.ReasonCode = reason.String
	var hangupReason sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT reason_code FROM call_events
		WHERE call_id = ? AND kind = ? AND reason_code IS NOT NULL AND reason_code <> ''
		ORDER BY seq DESC LIMIT 1`, callID, CallEventHangup).Scan(&hangupReason)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OutcomeFacts{}, fmt.Errorf("storage: load outcome hangup reason: %w", err)
	}
	if hangupReason.Valid {
		f.ReasonCode = hangupReason.String
	}

	var brief string
	err = s.db.QueryRowContext(ctx, `SELECT content_json FROM call_briefs
		WHERE call_id = ? AND owner_user_id = ?`, callID, owner).Scan(&brief)
	if errors.Is(err, sql.ErrNoRows) {
		// Every call has a brief, written with the call. A missing one is corrupt.
		return OutcomeFacts{}, fmt.Errorf("%w: missing brief", ErrOutcomeInputInvalid)
	}
	if err != nil {
		return OutcomeFacts{}, fmt.Errorf("storage: load outcome brief: %w", err)
	}
	var b struct {
		SuccessCriteria []string `json:"success_criteria"`
		Constraints     []string `json:"constraints"`
	}
	if err := json.Unmarshal([]byte(brief), &b); err != nil {
		return OutcomeFacts{}, fmt.Errorf("%w: undecodable brief", ErrOutcomeInputInvalid)
	}
	f.Criteria, f.Constraints = len(b.SuccessCriteria), len(b.Constraints)

	// A call is answered when an answered event was recorded, or when a final
	// callee turn shows the callee was reached (N1). A recorded transcript start
	// does not make a call answered: media can start while the call is still
	// ringing, before the callee picks up. A late answered event is therefore not
	// needed for a connected call to be judged on what was said. A final callee
	// turn does not count when the effective reason is no_answer or busy and no
	// answered event exists (N10, see below).
	var answered, mediaFailed, started, finalized, calleeFinal int
	if err := s.db.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM call_events WHERE call_id = ? AND kind = ?),
		EXISTS (SELECT 1 FROM call_events WHERE call_id = ? AND reason_code = ?),
		EXISTS (SELECT 1 FROM call_events WHERE call_id = ? AND kind = ?),
		EXISTS (SELECT 1 FROM call_events WHERE call_id = ? AND kind = ?),
		EXISTS (SELECT 1 FROM transcript_turns WHERE call_id = ? AND owner_user_id = ? AND speaker = 'callee' AND finality = 'final')`,
		callID, CallEventAnswered, callID, mediaFailedReason, callID, TranscriptStartedKind, callID, TranscriptFinalizedKind,
		callID, owner,
	).Scan(&answered, &mediaFailed, &started, &finalized, &calleeFinal); err != nil {
		return OutcomeFacts{}, fmt.Errorf("storage: load outcome flags: %w", err)
	}
	// N10: carrier early media can produce a final callee turn for a call that
	// never connected. When the effective reason says it was not answered
	// (no_answer or busy) and no answered event exists, the turn does not make it
	// answered. Live early-media behaviour is unverified (FEASIBILITY R19).
	calleeCounts := calleeFinal == 1 && (answered == 1 || !notConnectedNoAnswerOrBusy(f.ReasonCode))
	f.Answered = answered == 1 || calleeCounts
	f.MediaFailed = mediaFailed == 1
	f.TranscriptStarted, f.TranscriptFinalized = started == 1, finalized == 1

	turns, err := s.ListTranscriptTurns(ctx, owner, callID, 0, maxOutcomeTurns+1)
	if err != nil {
		return OutcomeFacts{}, err
	}
	if len(turns) > maxOutcomeTurns {
		return OutcomeFacts{}, ErrOutcomeTooLarge
	}
	f.Turns = turns

	if f.CallEvents, err = s.loadOutcomeCallEvents(ctx, callID); err != nil {
		return OutcomeFacts{}, err
	}
	if f.Assessments, err = s.loadOutcomeAssessments(ctx, callID); err != nil {
		return OutcomeFacts{}, err
	}
	return f, nil
}

func (s *Store) loadOutcomeCallEvents(ctx context.Context, callID string) ([]CallEventFact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, kind, COALESCE(reason_code, '') FROM call_events
		WHERE call_id = ? AND kind IN (?, ?, ?) ORDER BY seq`,
		callID, CallEventAnswered, CallEventHangup, CallEventFailed)
	if err != nil {
		return nil, fmt.Errorf("storage: load outcome call events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []CallEventFact
	for rows.Next() {
		var e CallEventFact
		if err := rows.Scan(&e.Seq, &e.Kind, &e.ReasonCode); err != nil {
			return nil, fmt.Errorf("storage: scan outcome call event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: load outcome call events: %w", err)
	}
	return out, nil
}

// loadOutcomeAssessments returns the newest maxOutcomeAssessments reports in seq
// order. Reports are only ever appended, so "last valid wins" is still decided
// on the newest reports.
func (s *Store) loadOutcomeAssessments(ctx context.Context, callID string) ([]AssessmentFact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, payload_json FROM (
		SELECT seq, payload_json FROM call_events WHERE call_id = ? AND kind = ?
		ORDER BY seq DESC LIMIT ?) ORDER BY seq`, callID, AssessmentReportedKind, maxOutcomeAssessments)
	if err != nil {
		return nil, fmt.Errorf("storage: load outcome assessments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AssessmentFact
	for rows.Next() {
		var a AssessmentFact
		if err := rows.Scan(&a.Seq, &a.PayloadJSON); err != nil {
			return nil, fmt.Errorf("storage: scan outcome assessment: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: load outcome assessments: %w", err)
	}
	return out, nil
}

// decodeOutcomeFields fills the task-outcome read fields of c. All four are
// NULL together, while the call is unassessed; the migration enforces that.
func decodeOutcomeFields(c *Call, reasonCodes, evidence, source, assessed sql.NullString) error {
	if !reasonCodes.Valid {
		return nil
	}
	if err := json.Unmarshal([]byte(reasonCodes.String), &c.OutcomeReasonCodes); err != nil {
		return fmt.Errorf("storage: decode outcome reason codes: %w", err)
	}
	if err := json.Unmarshal([]byte(evidence.String), &c.OutcomeEvidence); err != nil {
		return fmt.Errorf("storage: decode outcome evidence: %w", err)
	}
	c.OutcomeSource = source.String
	c.OutcomeAssessedAt = assessed.String
	return nil
}
