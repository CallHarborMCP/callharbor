package callworker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/outcome"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// OutcomeGrace is how long after ending a call with a started but unfinalized
// transcript is waited for before its outcome is evaluated anyway.
const OutcomeGrace = 2 * time.Minute

// outcomeStore is the storage surface the outcome evaluation uses. The worker
// checks for it at run time, so a store without it skips evaluation.
type outcomeStore interface {
	ListCallsAwaitingOutcome(ctx context.Context, now time.Time, grace time.Duration, limit int) ([]string, error)
	LoadOutcomeFacts(ctx context.Context, callID string) (storage.OutcomeFacts, error)
	RecordTaskOutcome(ctx context.Context, callID string, r outcome.Result, now time.Time) (bool, error)
}

// outcomeLoggedError marks an outcome error that evaluateOutcomes has already
// logged, so Run does not log the same failure a second time.
type outcomeLoggedError struct{ err error }

func (e outcomeLoggedError) Error() string { return e.err.Error() }
func (e outcomeLoggedError) Unwrap() error { return e.err }

// evaluateOutcomes evaluates a bounded batch of ready calls. Each call is
// evaluated on its own, so one poison call cannot block the others.
//
// A call whose input is permanently unreadable, or larger than the evaluation
// bound, is recorded unknown with a technical code, so it is never retried
// forever. A transient store error is retried on a later tick. Each error is
// logged once, with the call id and the error text only, and never with
// transcript or brief content. It returns the first error for the caller's
// own accounting; Tick does not log it again.
func (w *Worker) evaluateOutcomes(ctx context.Context) error {
	st, ok := w.store.(outcomeStore)
	if !ok {
		return nil
	}
	now := w.opts.Now()
	grace := w.opts.OutcomeGrace
	if grace == 0 {
		grace = OutcomeGrace
	}
	ids, err := st.ListCallsAwaitingOutcome(ctx, now, grace, w.opts.BatchSize)
	if err != nil {
		return fmt.Errorf("list awaiting outcome: %w", err)
	}
	var first error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return errors.Join(first, err)
		}
		if err := w.evaluateOne(ctx, st, id, now); err != nil {
			w.opts.Logger.Error("task outcome failed", "call_id", id, "error", err.Error())
			if first == nil {
				first = outcomeLoggedError{err: err}
			}
		}
	}
	return first
}

func (w *Worker) evaluateOne(ctx context.Context, st outcomeStore, callID string, now time.Time) error {
	facts, err := st.LoadOutcomeFacts(ctx, callID)
	var res outcome.Result
	switch {
	case errors.Is(err, storage.ErrOutcomeTooLarge):
		res = outcome.Unrecordable(outcome.CodeEvaluationBoundExceeded)
	case errors.Is(err, storage.ErrOutcomeInputInvalid):
		res = outcome.Unrecordable(outcome.CodeEvaluationInputInvalid)
	case err != nil:
		return fmt.Errorf("load facts: %w", err)
	default:
		res = outcome.Evaluate(outcomeInput(facts))
	}
	if _, err := st.RecordTaskOutcome(ctx, callID, res, now); err != nil {
		return fmt.Errorf("record outcome: %w", err)
	}
	return nil
}

// callEventFidelity is the fidelity cited for stored call events. Storage has no
// per-event fidelity; call events record provider signals, which are exact.
const callEventFidelity = "exact"

// outcomeInput maps stored facts to the evaluator's input. Storage gathers the
// rows; the mapping from stored kinds and reasons to evaluator codes lives here
// with the call state machine.
func outcomeInput(f storage.OutcomeFacts) outcome.Input {
	in := outcome.Input{
		Protocol:        f.Protocol,
		Answered:        f.Answered,
		ReasonCode:      f.ReasonCode,
		MediaFailed:     f.MediaFailed,
		CriteriaCount:   f.Criteria,
		ConstraintCount: f.Constraints,
		Transcript: outcome.Transcript{
			Started:   f.TranscriptStarted,
			Finalized: f.TranscriptFinalized,
		},
	}
	for _, t := range f.Turns {
		in.Transcript.Turns = append(in.Transcript.Turns, outcome.Turn{
			Seq: t.Seq, Speaker: t.Speaker, Finality: t.Finality, Text: t.Text, Fidelity: t.Fidelity,
		})
	}
	// Call events are provider signals the state machine recorded, not speech that
	// was heard or played, so no fidelity column exists for them in storage. They
	// are exact records of what the provider reported, and are cited as such.
	for _, e := range f.CallEvents {
		in.CallEvents = append(in.CallEvents, outcome.CallEventRef{Seq: e.Seq, Code: e.ReasonCode, Fidelity: callEventFidelity})
	}
	for _, a := range f.Assessments {
		// Invalid stored reports are skipped and counted, never trusted.
		if as, ok := outcome.AssessmentFromPayload(a.PayloadJSON, in.Transcript.Turns); ok {
			in.Assessments = append(in.Assessments, as)
		} else {
			in.InvalidAssessments++
		}
	}
	return in
}
