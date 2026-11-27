package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/callpolicy"
	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// policyOperator builds an operator whose installation policy is install. The
// fake store is returned so a test can check whether a row was written.
func policyOperator(t *testing.T, install config.CallPolicy, store *fakeCallStore) *CallOperator {
	t.Helper()
	op, err := NewCallOperator(store, CallDeps{
		Resolver: fakeResolver{p: readyProfile()}, MaxBriefBytes: 1 << 20, Policy: install,
		Now:       func() time.Time { return time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC) },
		NewCallID: func() (string, error) { return "call_test", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// requireRefusal asserts err is a service error with the given code and reason
// prefix, and that the store wrote nothing.
func requireRefusal(t *testing.T, store *fakeCallStore, err error, code Code, reason string) {
	t.Helper()
	if err == nil {
		t.Fatalf("MakeCall error = nil, want %s %s", code, reason)
	}
	if got := CodeOf(err); got != code {
		t.Fatalf("code = %s, want %s (err=%v)", got, code, err)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("error = %q, want reason %s", err.Error(), reason)
	}
	if len(store.calls) != 0 {
		t.Fatalf("refused call wrote %d rows", len(store.calls))
	}
}

func TestDenyPrefixRefusesBeforeAnyRow(t *testing.T) {
	store := newFakeCallStore(oneAI())
	op := policyOperator(t, config.CallPolicy{DenyPrefixes: []string{"+1415"}, MaxCallDurationSeconds: 7200}, store)
	_, err := op.MakeCall(context.Background(), callerAlice, validInput())
	requireRefusal(t, store, err, CodeForbidden, callpolicy.ReasonDestinationDenied)
}

func TestAllowListRequiresAMatch(t *testing.T) {
	store := newFakeCallStore(oneAI())
	op := policyOperator(t, config.CallPolicy{AllowPrefixes: []string{"+44"}, MaxCallDurationSeconds: 7200}, store)
	_, err := op.MakeCall(context.Background(), callerAlice, validInput())
	requireRefusal(t, store, err, CodeForbidden, callpolicy.ReasonDestinationNotAllowed)
}

func TestOwnerDenyCannotBeOverriddenByInstallationAllow(t *testing.T) {
	store := newFakeCallStore(oneAI())
	store.policies = map[string]storage.UserCallPolicy{
		"alice": {UserID: "alice", DenyPrefixes: []string{"+1415"}},
	}
	op := policyOperator(t, config.CallPolicy{AllowPrefixes: []string{"+1"}, MaxCallDurationSeconds: 7200}, store)
	_, err := op.MakeCall(context.Background(), callerAlice, validInput())
	requireRefusal(t, store, err, CodeForbidden, callpolicy.ReasonDestinationDenied)
}

func TestOwnerAllowMustAlsoMatchInstallationAllow(t *testing.T) {
	store := newFakeCallStore(oneAI())
	store.policies = map[string]storage.UserCallPolicy{
		"alice": {UserID: "alice", AllowPrefixes: []string{"+1415"}},
	}
	// The installation allows only +44, so +1415 matches the owner group but
	// not the installation group. Both groups must match.
	op := policyOperator(t, config.CallPolicy{AllowPrefixes: []string{"+44"}, MaxCallDurationSeconds: 7200}, store)
	_, err := op.MakeCall(context.Background(), callerAlice, validInput())
	requireRefusal(t, store, err, CodeForbidden, callpolicy.ReasonDestinationNotAllowed)
}

func TestExplicitDurationAbovePolicyIsRefused(t *testing.T) {
	store := newFakeCallStore(oneAI())
	op := policyOperator(t, config.CallPolicy{MaxCallDurationSeconds: 300}, store)
	in := validInput()
	long := 600
	in.MaxDuration = &long
	_, err := op.MakeCall(context.Background(), callerAlice, in)
	requireRefusal(t, store, err, CodeInvalidInput, callpolicy.ReasonDurationExceedsPolicy)
}

func TestOmittedDurationIsClampedToPolicy(t *testing.T) {
	store := newFakeCallStore(oneAI())
	op := policyOperator(t, config.CallPolicy{MaxCallDurationSeconds: 120}, store)
	if _, err := op.MakeCall(context.Background(), callerAlice, validInput()); err != nil {
		t.Fatalf("MakeCall: %v", err)
	}
	for _, c := range store.calls {
		if c.MaxDurationSeconds != 120 {
			t.Fatalf("duration = %d, want the policy clamp 120", c.MaxDurationSeconds)
		}
	}
}

func TestOwnerDurationOverrideTightensInstallation(t *testing.T) {
	store := newFakeCallStore(oneAI())
	store.policies = map[string]storage.UserCallPolicy{
		"alice": {UserID: "alice", MaxDurationSeconds: int64Ptr(60)},
	}
	op := policyOperator(t, config.CallPolicy{MaxCallDurationSeconds: 7200}, store)
	in := validInput()
	long := 120
	in.MaxDuration = &long
	_, err := op.MakeCall(context.Background(), callerAlice, in)
	requireRefusal(t, store, err, CodeInvalidInput, callpolicy.ReasonDurationExceedsPolicy)
}

func TestOwnerDurationOverrideCannotLoosenInstallation(t *testing.T) {
	store := newFakeCallStore(oneAI())
	store.policies = map[string]storage.UserCallPolicy{
		"alice": {UserID: "alice", MaxDurationSeconds: int64Ptr(7200)},
	}
	op := policyOperator(t, config.CallPolicy{MaxCallDurationSeconds: 300}, store)
	in := validInput()
	long := 600
	in.MaxDuration = &long
	_, err := op.MakeCall(context.Background(), callerAlice, in)
	requireRefusal(t, store, err, CodeInvalidInput, callpolicy.ReasonDurationExceedsPolicy)
}

func TestCostLimitWithoutRateIsRefusedNeverDialed(t *testing.T) {
	store := newFakeCallStore(oneAI())
	limit := int64(1_000_000)
	op := policyOperator(t, config.CallPolicy{MaxCallDurationSeconds: 7200, MaxCallCostMicros: &limit, CallCostCurrency: "USD"}, store)
	_, err := op.MakeCall(context.Background(), callerAlice, validInput())
	requireRefusal(t, store, err, CodeConflict, callpolicy.ReasonCostUnavailable)
}

func TestCostEstimateAboveLimitIsRefused(t *testing.T) {
	store := newFakeCallStore(oneAI())
	limit := int64(1_000_000) // 1.00
	rate := int64(250_000)    // 0.25 per minute
	op := policyOperator(t, config.CallPolicy{
		MaxCallDurationSeconds: 7200, MaxCallCostMicros: &limit,
		CallCostEstimateMicrosPerMinute: &rate, CallCostCurrency: "USD",
	}, store)
	in := validInput()
	long := 300 // 5 minutes × 0.25 = 1.25, above 1.00
	in.MaxDuration = &long
	_, err := op.MakeCall(context.Background(), callerAlice, in)
	requireRefusal(t, store, err, CodeForbidden, callpolicy.ReasonCostLimitExceeded)
}

func TestCostEstimateRoundsPartialMinutesUpAndIsRecorded(t *testing.T) {
	store := newFakeCallStore(oneAI())
	rate := int64(250_000) // 0.25 per minute
	op := policyOperator(t, config.CallPolicy{
		MaxCallDurationSeconds: 7200, CallCostEstimateMicrosPerMinute: &rate, CallCostCurrency: "USD",
	}, store)
	in := validInput()
	short := 61 // ceil(61/60) = 2 minutes = 0.50
	in.MaxDuration = &short
	if _, err := op.MakeCall(context.Background(), callerAlice, in); err != nil {
		t.Fatalf("MakeCall: %v", err)
	}
	cost := store.lastCost
	if cost.Status != "estimated" || cost.AmountMicros == nil || *cost.AmountMicros != 500_000 || cost.Currency != "USD" {
		t.Fatalf("cost = %+v, want estimated 500000 USD", cost)
	}
}

func TestNoRateAndNoLimitRecordsUnavailableCost(t *testing.T) {
	store := newFakeCallStore(oneAI())
	op := policyOperator(t, config.CallPolicy{MaxCallDurationSeconds: 7200}, store)
	if _, err := op.MakeCall(context.Background(), callerAlice, validInput()); err != nil {
		t.Fatalf("MakeCall: %v", err)
	}
	if store.lastCost.Status != "unavailable" {
		t.Fatalf("cost status = %q, want unavailable", store.lastCost.Status)
	}
}

func TestConcurrencyLimitMapsToStableCodeAndWritesNothing(t *testing.T) {
	store := newFakeCallStore(oneAI())
	store.limitErr = storage.ErrOwnerConcurrencyLimit
	op := policyOperator(t, config.CallPolicy{MaxCallDurationSeconds: 7200}, store)
	_, err := op.MakeCall(context.Background(), callerAlice, validInput())
	requireRefusal(t, store, err, CodeConflict, callpolicy.ReasonConcurrencyLimit)
}

func TestGlobalConcurrencyLimitMapsToStableCode(t *testing.T) {
	store := newFakeCallStore(oneAI())
	store.limitErr = storage.ErrGlobalConcurrencyLimit
	op := policyOperator(t, config.CallPolicy{MaxCallDurationSeconds: 7200}, store)
	_, err := op.MakeCall(context.Background(), callerAlice, validInput())
	requireRefusal(t, store, err, CodeConflict, callpolicy.ReasonGlobalConcurrencyLimit)
}

func TestRateLimitMapsToRateLimitedCode(t *testing.T) {
	store := newFakeCallStore(oneAI())
	store.limitErr = storage.ErrOwnerRateLimit
	op := policyOperator(t, config.CallPolicy{MaxCallDurationSeconds: 7200}, store)
	_, err := op.MakeCall(context.Background(), callerAlice, validInput())
	requireRefusal(t, store, err, CodeRateLimited, callpolicy.ReasonRateLimited)
	if got := CodeRateLimited.HTTPStatus(); got != 429 {
		t.Fatalf("rate_limited status = %d, want 429", got)
	}
}

// TestReplayIsNotRefusedByLaterTightenedPolicy: a retry of an existing call
// returns it unchanged, even when the policy has since become stricter.
func TestReplayIsNotRefusedByLaterTightenedPolicy(t *testing.T) {
	store := newFakeCallStore(oneAI())
	op := policyOperator(t, config.CallPolicy{MaxCallDurationSeconds: 7200}, store)
	first, err := op.MakeCall(context.Background(), callerAlice, validInput())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// The policy now denies the destination. A replay must still return the call.
	op = policyOperator(t, config.CallPolicy{DenyPrefixes: []string{"+1415"}, MaxCallDurationSeconds: 7200}, store)
	again, err := op.MakeCall(context.Background(), callerAlice, validInput())
	if err != nil {
		t.Fatalf("replay refused: %v", err)
	}
	if again.CallID != first.CallID {
		t.Fatalf("replay call = %s, want %s", again.CallID, first.CallID)
	}
}

// TestReplayOfClampedOmittedDurationReturnsSameCall: an omitted duration is
// clamped to the policy (120s here). A retry of the same request must return
// the stored call, not an idempotency conflict with the unclamped 600s default.
func TestReplayOfClampedOmittedDurationReturnsSameCall(t *testing.T) {
	store := newFakeCallStore(oneAI())
	op := policyOperator(t, config.CallPolicy{MaxCallDurationSeconds: 120}, store)
	first, err := op.MakeCall(context.Background(), callerAlice, validInput())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// The policy max (120) is below the 600s default, so the clamp really
	// happened: the stored row holds 120, not the default.
	if got := store.calls[first.CallID].MaxDurationSeconds; got != 120 {
		t.Fatalf("stored duration = %d, want the clamp 120 (policy max < 600 default)", got)
	}
	again, err := op.MakeCall(context.Background(), callerAlice, validInput())
	if err != nil {
		t.Fatalf("replay of clamped call refused: %v", err)
	}
	if again.CallID != first.CallID {
		t.Fatalf("replay call = %s, want %s", again.CallID, first.CallID)
	}
	if store.inserts != 1 {
		t.Fatalf("inserts = %d, want 1 (replay must not write)", store.inserts)
	}
}

// TestClampDoesNotChangeCanonicalBriefOrSHA: the duration is not part of the
// canonical brief, so a clamp leaves the brief JSON and its digest unchanged.
// The same request under a permissive policy has the same digest.
func TestClampDoesNotChangeCanonicalBriefOrSHA(t *testing.T) {
	clamped := newFakeCallStore(oneAI())
	if _, err := policyOperator(t, config.CallPolicy{MaxCallDurationSeconds: 120}, clamped).
		MakeCall(context.Background(), callerAlice, validInput()); err != nil {
		t.Fatal(err)
	}
	open := newFakeCallStore(oneAI())
	if _, err := policyOperator(t, config.CallPolicy{MaxCallDurationSeconds: 7200}, open).
		MakeCall(context.Background(), callerAlice, validInput()); err != nil {
		t.Fatal(err)
	}
	var clampedBrief, openBrief storage.CallBrief
	for _, b := range clamped.briefs {
		clampedBrief = b
	}
	for _, b := range open.briefs {
		openBrief = b
	}
	if clampedBrief.SHA256 == "" || clampedBrief.SHA256 != openBrief.SHA256 {
		t.Fatalf("clamp changed the brief digest: %q vs %q", clampedBrief.SHA256, openBrief.SHA256)
	}
	if clampedBrief.ContentJSON != openBrief.ContentJSON {
		t.Fatal("clamp changed the canonical brief JSON")
	}
}

// TestReplayExplicitDurationStillConflictsOnChange: an explicit duration is
// compared exactly, so a different explicit value under the same key conflicts.
func TestReplayExplicitDurationStillConflictsOnChange(t *testing.T) {
	store := newFakeCallStore(oneAI())
	op := policyOperator(t, config.CallPolicy{MaxCallDurationSeconds: 7200}, store)
	first := validInput()
	d1 := 300
	first.MaxDuration = &d1
	if _, err := op.MakeCall(context.Background(), callerAlice, first); err != nil {
		t.Fatalf("first: %v", err)
	}
	second := validInput()
	d2 := 301
	second.MaxDuration = &d2
	_, err := op.MakeCall(context.Background(), callerAlice, second)
	if CodeOf(err) != CodeConflict {
		t.Fatalf("code = %s, want conflict (err=%v)", CodeOf(err), err)
	}
}

func TestMapLimitErrorLeavesOtherErrorsAlone(t *testing.T) {
	if got := mapLimitError(errors.New("disk full")); got != nil {
		t.Fatalf("unrelated error mapped to %v", got)
	}
}

func int64Ptr(v int64) *int64 { return &v }
