package service

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// hangupStore wraps the shared fake so a test can choose the decision
// RequestHangup returns. The fake's own RequestHangup always reports none.
type hangupStore struct {
	*fakeCallStore
	decision storage.HangupDecision
}

func (h *hangupStore) RequestHangup(ctx context.Context, callID string, audit storage.AuditEvent, now time.Time) (storage.HangupDecision, error) {
	if _, err := h.CallOwner(ctx, callID); err != nil {
		return storage.HangupDecision{}, err
	}
	h.audits = append(h.audits, audit)
	return h.decision, nil
}

// seedCall stores one call owned by owner, in the given status.
func seedCall(f *fakeCallStore, id, owner, status string) {
	f.calls[id] = storage.Call{ID: id, OwnerUserID: owner, Status: status, TelephonyProfileID: "tp-1", ToNumber: "+14155550111"}
}

var (
	callerCarol = Actor{UserID: "carol", Scopes: []string{ScopeRead, ScopeCall}}
	callerAdmin = Actor{UserID: "root", Scopes: []string{ScopeRead, ScopeCall}, Admin: true}
)

func TestHangupCallOwnerSucceedsWithOwnAuditRow(t *testing.T) {
	store := newFakeCallStore(oneAI())
	seedCall(store, "call_1", "alice", "active")
	op := newOperator(t, store, fakeResolver{p: readyProfile()})

	res, err := op.HangupCall(context.Background(), callerAlice, "call_1")
	if err != nil {
		t.Fatalf("owner hangup: %v", err)
	}
	if res.CallID != "call_1" || res.TechnicalStatus != "active" {
		t.Fatalf("result = %+v", res)
	}
	if len(store.audits) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(store.audits))
	}
	a := store.audits[0]
	if a.Action != "call.hangup" || a.Outcome != "success" || a.ActorUserID != "alice" || a.ActorRole != "user" {
		t.Fatalf("owner audit = %+v", a)
	}
	if a.TargetUserID != "" {
		t.Fatalf("owner audit target = %q, want empty", a.TargetUserID)
	}
}

func TestHangupCallAdminOnOtherUsersCallRecordsTarget(t *testing.T) {
	store := newFakeCallStore(oneAI())
	seedCall(store, "call_2", "alice", "ringing")
	op := newOperator(t, store, fakeResolver{p: readyProfile()})

	if _, err := op.HangupCall(context.Background(), callerAdmin, "call_2"); err != nil {
		t.Fatalf("admin hangup: %v", err)
	}
	if len(store.audits) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(store.audits))
	}
	a := store.audits[0]
	if a.Outcome != "success" || a.ActorUserID != "root" || a.ActorRole != "admin" {
		t.Fatalf("admin audit = %+v", a)
	}
	if a.TargetUserID != "alice" {
		t.Fatalf("admin audit target = %q, want alice", a.TargetUserID)
	}
}

func TestHangupCallOtherUserGetsNotFoundAndDeniedAudit(t *testing.T) {
	store := newFakeCallStore(oneAI())
	seedCall(store, "call_3", "alice", "active")
	op := newOperator(t, store, fakeResolver{p: readyProfile()})

	_, err := op.HangupCall(context.Background(), callerCarol, "call_3")
	if CodeOf(err) != CodeNotFound {
		t.Fatalf("other user: err = %v, want not_found", err)
	}
	if len(store.audits) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(store.audits))
	}
	a := store.audits[0]
	if a.Outcome != "denied" || a.ActorUserID != "carol" || a.Action != "call.hangup" {
		t.Fatalf("denied audit = %+v", a)
	}
	// The refusal must not reveal the owner, so the row carries no target.
	if a.TargetUserID != "" {
		t.Fatalf("denied audit target = %q, want empty", a.TargetUserID)
	}
	if store.calls["call_3"].Status != "active" {
		t.Fatalf("refused hangup changed status to %q", store.calls["call_3"].Status)
	}
}

func TestHangupCallMissingCallIsNotFound(t *testing.T) {
	store := newFakeCallStore(oneAI())
	op := newOperator(t, store, fakeResolver{p: readyProfile()})

	_, err := op.HangupCall(context.Background(), callerAlice, "call_missing")
	if CodeOf(err) != CodeNotFound {
		t.Fatalf("missing call: err = %v, want not_found", err)
	}
}

func TestHangupCallReadOnlyScopeIsInsufficient(t *testing.T) {
	store := newFakeCallStore(oneAI())
	seedCall(store, "call_4", "alice", "active")
	op := newOperator(t, store, fakeResolver{p: readyProfile()})

	readOnly := Actor{UserID: "alice", Scopes: []string{ScopeRead}}
	_, err := op.HangupCall(context.Background(), readOnly, "call_4")
	if CodeOf(err) != CodeInsufficientScope {
		t.Fatalf("read-only owner: err = %v, want insufficient_scope", err)
	}
	if len(store.audits) != 0 {
		t.Fatalf("scope refusal wrote %d audit rows, want none", len(store.audits))
	}
	if store.calls["call_4"].Status != "active" {
		t.Fatalf("scope refusal changed status to %q", store.calls["call_4"].Status)
	}
}

func TestHangupCallUnauthenticatedIsRefused(t *testing.T) {
	store := newFakeCallStore(oneAI())
	seedCall(store, "call_5", "alice", "active")
	op := newOperator(t, store, fakeResolver{p: readyProfile()})

	_, err := op.HangupCall(context.Background(), Actor{}, "call_5")
	if CodeOf(err) != CodeUnauthenticated {
		t.Fatalf("no actor: err = %v, want unauthenticated", err)
	}
}

func TestHangupCallSendsLegWhenStoreDecidesSo(t *testing.T) {
	store := &hangupStore{
		fakeCallStore: newFakeCallStore(oneAI()),
		decision: storage.HangupDecision{
			Action: storage.HangupActionSendLeg, Status: "ending",
			Leg: storage.LiveCallLeg{ID: "call_6", ControlID: "v3:ctl-6", TelephonyOwnerUserID: "alice"},
		},
	}
	seedCall(store.fakeCallStore, "call_6", "alice", "active")
	var sent []storage.LiveCallLeg
	op, err := NewCallOperator(store, CallDeps{
		Resolver: fakeResolver{p: readyProfile()}, MaxBriefBytes: 1 << 20,
		Now:       func() time.Time { return time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC) },
		NewCallID: func() (string, error) { return "call_test", nil },
		Hangup: func(_ context.Context, leg storage.LiveCallLeg) error {
			sent = append(sent, leg)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := op.HangupCall(context.Background(), callerAlice, "call_6")
	if err != nil {
		t.Fatalf("hangup: %v", err)
	}
	if res.TechnicalStatus != "ending" {
		t.Fatalf("status = %q, want ending", res.TechnicalStatus)
	}
	if len(sent) != 1 || sent[0].ControlID != "v3:ctl-6" {
		t.Fatalf("sent legs = %+v, want one leg with the control id", sent)
	}
}

func TestHangupCallSendFailureIsLoggedWithCallIDOnly(t *testing.T) {
	const number = "+14155550111"
	const control = "v3:secret-control-id"
	store := &hangupStore{
		fakeCallStore: newFakeCallStore(oneAI()),
		decision: storage.HangupDecision{
			Action: storage.HangupActionSendLeg, Status: "ending",
			Leg: storage.LiveCallLeg{ID: "call_7", ControlID: control, TelephonyOwnerUserID: "alice"},
		},
	}
	seedCall(store.fakeCallStore, "call_7", "alice", "active")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	op, err := NewCallOperator(store, CallDeps{
		Resolver: fakeResolver{p: readyProfile()}, MaxBriefBytes: 1 << 20,
		Now:       func() time.Time { return time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC) },
		NewCallID: func() (string, error) { return "call_test", nil },
		Logger:    logger,
		Hangup: func(context.Context, storage.LiveCallLeg) error {
			// A provider error can echo request detail. It must never reach the log.
			return errors.New("telnyx 500 for " + number + " key sk-live-abc " + control)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := op.HangupCall(context.Background(), callerAlice, "call_7")
	if err != nil {
		t.Fatalf("a failed send must not fail the caller's hangup: %v", err)
	}
	if res.TechnicalStatus != "ending" {
		t.Fatalf("status = %q, want ending", res.TechnicalStatus)
	}
	logged := buf.String()
	if !strings.Contains(logged, "call_7") || !strings.Contains(logged, "call hangup send failed") {
		t.Fatalf("log = %q, want the failure with call_id", logged)
	}
	for _, secret := range []string{number, "sk-live-abc", control} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaks %q: %s", secret, logged)
		}
	}
}

func TestHangupCallNilLoggerDiscardsSendFailure(t *testing.T) {
	store := &hangupStore{
		fakeCallStore: newFakeCallStore(oneAI()),
		decision: storage.HangupDecision{
			Action: storage.HangupActionSendLeg, Status: "ending",
			Leg: storage.LiveCallLeg{ID: "call_8", ControlID: "v3:ctl-8", TelephonyOwnerUserID: "alice"},
		},
	}
	seedCall(store.fakeCallStore, "call_8", "alice", "active")
	op, err := NewCallOperator(store, CallDeps{
		Resolver: fakeResolver{p: readyProfile()}, MaxBriefBytes: 1 << 20,
		Now:       func() time.Time { return time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC) },
		NewCallID: func() (string, error) { return "call_test", nil },
		Hangup:    func(context.Context, storage.LiveCallLeg) error { return errors.New("down") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.HangupCall(context.Background(), callerAlice, "call_8"); err != nil {
		t.Fatalf("nil logger: %v", err)
	}
}
