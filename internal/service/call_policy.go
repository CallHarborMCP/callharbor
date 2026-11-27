package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/CallHarborMCP/callharbor/internal/callpolicy"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// effectivePolicy returns the installation policy combined with the owner's
// overrides. A read error is internal: the call is refused, never dialed.
func (o *CallOperator) effectivePolicy(ctx context.Context, userID string) (callpolicy.Effective, error) {
	user, err := o.store.GetUserCallPolicy(ctx, userID)
	if err != nil {
		return callpolicy.Effective{}, internal(fmt.Errorf("read user call policy: %w", err))
	}
	return callpolicy.Resolve(o.deps.Policy, user), nil
}

// checkCallPolicy refuses a request the effective policy forbids. It runs
// before any row is written, so a refusal leaves nothing to dial. An explicit
// duration above the policy limit is refused. An omitted duration is clamped
// to the limit, so a default never exceeds what the policy allows.
func checkCallPolicy(e callpolicy.Effective, r RenderedBrief, explicitDuration bool) (RenderedBrief, error) {
	switch reason := callpolicy.CheckDestination(e, r.NormalizedTo); reason {
	case callpolicy.ReasonDestinationDenied:
		return r, newError(CodeForbidden, reason+": calls to this destination are not permitted by the installation or your call policy")
	case callpolicy.ReasonDestinationNotAllowed:
		return r, newError(CodeForbidden, reason+": the destination is not in the allowed call prefixes")
	}
	if r.MaxDuration > e.MaxDurationSeconds {
		if explicitDuration {
			return r, newError(CodeInvalidInput, fmt.Sprintf(
				"%s: max_duration_seconds %d exceeds the policy limit of %d seconds",
				callpolicy.ReasonDurationExceedsPolicy, r.MaxDuration, e.MaxDurationSeconds))
		}
		r.MaxDuration = e.MaxDurationSeconds
	}
	return r, nil
}

// limitsFor converts the effective concurrency and rate limits to the storage
// check. Zero means the limit is not checked.
func limitsFor(e callpolicy.Effective) storage.CallLimits {
	return storage.CallLimits{
		OwnerMaxConcurrent:  e.MaxConcurrentCalls,
		GlobalMaxConcurrent: e.GlobalMaxConcurrent,
		OwnerMaxPerHour:     e.MaxCallsPerHour,
	}
}

// costEstimate builds the pre-dial cost record. With a cost limit set, the
// estimate must exist and fit under it, or the call is refused: an unknown
// cost is never dialed. Without a limit, a configured rate is still recorded
// as an estimate and no rate is recorded as unavailable.
func costEstimate(e callpolicy.Effective, durationSeconds int) (storage.CallCost, error) {
	if e.EstimateMicrosPerMinute == nil {
		if e.MaxCostMicros != nil {
			return storage.CallCost{}, newError(CodeConflict, callpolicy.ReasonCostUnavailable+
				": a cost limit is set but no estimate rate is configured (CALLHARBOR_CALL_COST_ESTIMATE_PER_MINUTE)")
		}
		return storage.CallCost{Status: "unavailable"}, nil
	}
	amount, ok := callpolicy.EstimateMicros(*e.EstimateMicrosPerMinute, durationSeconds)
	if !ok {
		return storage.CallCost{}, newError(CodeConflict, callpolicy.ReasonCostUnavailable+
			": the cost estimate does not fit in the ledger")
	}
	if e.MaxCostMicros != nil && amount > *e.MaxCostMicros {
		return storage.CallCost{}, newError(CodeForbidden, callpolicy.ReasonCostLimitExceeded+
			": the estimated cost exceeds the per-call cost limit")
	}
	cur := e.CostCurrency
	return storage.CallCost{Status: "estimated", AmountMicros: &amount, Currency: cur}, nil
}

// mapLimitError turns a storage limit refusal into its stable reason code.
// Anything else returns nil so the caller handles it as before.
func mapLimitError(err error) error {
	switch {
	case errors.Is(err, storage.ErrOwnerConcurrencyLimit):
		return newError(CodeConflict, callpolicy.ReasonConcurrencyLimit+
			": you already have the maximum number of calls in progress")
	case errors.Is(err, storage.ErrGlobalConcurrencyLimit):
		return newError(CodeConflict, callpolicy.ReasonGlobalConcurrencyLimit+
			": the installation has the maximum number of calls in progress")
	case errors.Is(err, storage.ErrOwnerRateLimit):
		return newError(CodeRateLimited, callpolicy.ReasonRateLimited+
			": too many calls were placed in the last hour")
	default:
		return nil
	}
}
