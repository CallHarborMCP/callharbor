package callworker

import (
	"context"
	"errors"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/callpolicy"
)

// TestDialTimeDenyTightenedAfterQueueSendsNothing covers a destination that
// was allowed when the call was queued and is denied by the policy in force at
// dial time. The worker must fail the call with destination_denied and must not
// dial.
func TestDialTimeDenyTightenedAfterQueueSendsNothing(t *testing.T) {
	deny := callpolicy.Effective{DenyPrefixes: []string{"+1415"}}
	p := planner(t, readyResolved(), okRoutes("https://ch.example.com"))
	p.Policy = fixedPolicy{e: deny}

	store := newFakeStore(call("c-deny"))
	dialer := &fakeDialer{}
	p.Dialers = func(string) (Dialer, error) { return dialer, nil }
	w := newWorker(t, store, p, true)

	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n := len(dialer.dials); n != 0 {
		t.Fatalf("dials = %d, want 0 for a destination the current policy denies", n)
	}
	if store.status["c-deny"] != "failed" || store.reasons["c-deny"] != callpolicy.ReasonDestinationDenied {
		t.Fatalf("status=%q reason=%q, want failed/%s",
			store.status["c-deny"], store.reasons["c-deny"], callpolicy.ReasonDestinationDenied)
	}
}

// TestDialWithoutPolicySourceFailsClosed covers a worker built with no policy
// source. It must refuse every dial rather than dial unchecked.
func TestDialWithoutPolicySourceFailsClosed(t *testing.T) {
	p := planner(t, readyResolved(), okRoutes("https://ch.example.com"))
	p.Policy = nil

	store := newFakeStore(call("c-open"))
	dialer := &fakeDialer{}
	p.Dialers = func(string) (Dialer, error) { return dialer, nil }
	w := newWorker(t, store, p, true)

	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n := len(dialer.dials); n != 0 {
		t.Fatalf("dials = %d, want 0 without a policy source", n)
	}
	if store.reasons["c-open"] != ReasonPolicyUnavailable {
		t.Fatalf("reason = %q, want %s", store.reasons["c-open"], ReasonPolicyUnavailable)
	}
}

// TestDialPolicyReadErrorIsNotSwallowed covers a policy read failure. The call
// is not dialed, and the error is returned, not converted into a permissive
// plan.
func TestDialPolicyReadErrorIsNotSwallowed(t *testing.T) {
	boom := errors.New("policy db down")
	p := planner(t, readyResolved(), okRoutes("https://ch.example.com"))
	p.Policy = fixedPolicy{err: boom}

	if _, reason, err := p.Plan(context.Background(), call("c")); !errors.Is(err, boom) || reason != "" {
		t.Fatalf("plan err=%v reason=%q, want the policy error and no reason", err, reason)
	}
}

// TestDialAllowedDestinationStillDials confirms the dial-time check does not
// block a destination the policy allows.
func TestDialAllowedDestinationStillDials(t *testing.T) {
	allow := callpolicy.Effective{AllowGroups: [][]string{{"+1"}}}
	p := planner(t, readyResolved(), okRoutes("https://ch.example.com"))
	p.Policy = fixedPolicy{e: allow}

	if _, reason, err := p.Plan(context.Background(), call("c")); err != nil || reason != "" {
		t.Fatalf("plan reason=%q err=%v, want a plan", reason, err)
	}
}
