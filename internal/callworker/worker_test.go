package callworker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

var fixedNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// fakeStore is an in-memory Store. Its claim is a conditional update, so a
// second claim of the same call reports not claimable, as the real store does.
type fakeStore struct {
	mu         sync.Mutex
	queued     []storage.QueuedCallRef
	status     map[string]string
	controlID  map[string]string
	reasons    map[string]string
	hangupSent map[string]int
	unconf     []string
}

func newFakeStore(calls ...storage.QueuedCallRef) *fakeStore {
	s := &fakeStore{status: map[string]string{}, controlID: map[string]string{}, reasons: map[string]string{}, hangupSent: map[string]int{}}
	for _, c := range calls {
		s.queued = append(s.queued, c)
		s.status[c.ID] = "queued"
	}
	return s
}

func (s *fakeStore) ListQueuedCalls(_ context.Context, limit int) ([]storage.QueuedCallRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []storage.QueuedCallRef
	for _, c := range s.queued {
		if s.status[c.ID] == "queued" && len(out) < limit {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *fakeStore) ListUnconfirmedInitiating(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.unconf...), nil
}

func (s *fakeStore) ListLiveCallLegs(context.Context) ([]storage.LiveCallLeg, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []storage.LiveCallLeg
	for id, st := range s.status {
		if cid := s.controlID[id]; cid != "" && (st == "initiating" || st == "ringing" || st == "active" || st == "ending") {
			out = append(out, storage.LiveCallLeg{ID: id, OwnerUserID: "owner", ControlID: cid})
		}
	}
	return out, nil
}

func (s *fakeStore) ClaimQueuedCall(_ context.Context, id string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status[id] != "queued" {
		return storage.ErrCallNotClaimable
	}
	s.status[id] = "initiating"
	return nil
}

func (s *fakeStore) MarkCallFailed(_ context.Context, id, reason string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status[id] != "queued" && s.status[id] != "initiating" {
		return errors.New("not failable")
	}
	s.status[id] = "failed"
	s.reasons[id] = reason
	return nil
}

func (s *fakeStore) RecordProviderCallControlID(_ context.Context, id, _, control string, _ time.Time) (storage.LiveCallLeg, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status[id]
	if st != "initiating" && st != "ending" {
		return storage.LiveCallLeg{}, "", errors.New("not initiating")
	}
	s.controlID[id] = control
	return storage.LiveCallLeg{ID: id, OwnerUserID: "owner", ControlID: control}, st, nil
}

// BeginLocalHangup mirrors the storage contract for the fake: a live call moves
// to ending with the reason, and the leg is returned only when it had a
// control id.
func (s *fakeStore) BeginLocalHangup(_ context.Context, id, reason string, _ time.Time) (storage.LiveCallLeg, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status[id]
	if st != "initiating" && st != "ringing" && st != "active" {
		return storage.LiveCallLeg{}, false, nil
	}
	s.status[id] = "ending"
	s.reasons[id] = reason
	return storage.LiveCallLeg{ID: id, OwnerUserID: "owner", ControlID: s.controlID[id]}, true, nil
}

// RecordHangupSent counts hangups sent per call, so tests can assert the
// marker is written for every send.
func (s *fakeStore) RecordHangupSent(_ context.Context, id string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hangupSent[id]++
	return nil
}

func (s *fakeStore) ListEndingForReconcile(context.Context, time.Time, int) ([]storage.ReconcileCall, error) {
	return nil, nil
}

func (s *fakeStore) ListOverdueCalls(context.Context, time.Time, int, int) ([]storage.ReconcileCall, error) {
	return nil, nil
}

func (s *fakeStore) ListOrphanedTerminal(context.Context, time.Time, int, int) ([]storage.ReconcileCall, error) {
	return nil, nil
}

func (s *fakeStore) MarkReconcileEnded(_ context.Context, id, reason string, _ time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status[id] != "ending" {
		return false, nil
	}
	s.status[id] = "ended"
	if reason != "" {
		s.reasons[id] = reason
	}
	return true, nil
}

// fakeDialer counts dials and can be told to fail.
type fakeDialer struct {
	mu     sync.Mutex
	dials  []telnyx.DialParams
	err    error
	result telnyx.DialResult
}

func (d *fakeDialer) Dial(_ context.Context, p telnyx.DialParams) (telnyx.DialResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials = append(d.dials, p)
	if d.err != nil {
		return telnyx.DialResult{}, d.err
	}
	return d.result, nil
}

// fixedPlanner returns one plan for every call, or a fixed refusal reason.
type fixedPlanner struct {
	dialer *fakeDialer
	reason string
	err    error
}

func (p fixedPlanner) Plan(context.Context, storage.QueuedCallRef) (Plan, string, error) {
	if p.err != nil {
		return Plan{}, "", p.err
	}
	if p.reason != "" {
		return Plan{}, p.reason, nil
	}
	return Plan{Dialer: p.dialer, From: "+14155550100", ConnectionID: "conn-1",
		WebhookURL: "https://pub.example/hook", StreamURL: "wss://pub.example/stream", MaxSeconds: 600}, "", nil
}

func call(id string) storage.QueuedCallRef {
	return storage.QueuedCallRef{ID: id, OwnerUserID: "alice", TelephonyProfileID: "tp", AIProfileID: "ai",
		ToNumber: "+14155550111", MaxDurationSeconds: 600, CreatedAt: "2026-11-25T11:00:00.000Z"}
}

func newWorker(t *testing.T, store Store, planner Planner, enabled bool) *Worker {
	t.Helper()
	w, err := New(store, planner, Options{Enabled: enabled, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestDisabledWorkerNeverDials(t *testing.T) {
	store := newFakeStore(call("c1"))
	d := &fakeDialer{result: telnyx.DialResult{CallControlID: "cc"}}
	w := newWorker(t, store, fixedPlanner{dialer: d}, false)
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.dials) != 0 || store.status["c1"] != "queued" {
		t.Fatalf("disabled worker dialed %d times, status %s", len(d.dials), store.status["c1"])
	}
}

func TestEachQueuedCallIsDialedExactlyOnce(t *testing.T) {
	store := newFakeStore(call("c1"), call("c2"))
	d := &fakeDialer{result: telnyx.DialResult{CallControlID: "cc"}}
	w := newWorker(t, store, fixedPlanner{dialer: d}, true)
	for i := 0; i < 3; i++ {
		if err := w.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.dials) != 2 {
		t.Fatalf("dials = %d, want 2 (one per call)", len(d.dials))
	}
	seen := map[string]int{}
	for _, p := range d.dials {
		seen[p.CommandID]++
	}
	for _, id := range []string{"c1", "c2"} {
		if seen[telnyx.CommandID(id)] != 1 {
			t.Fatalf("command id for %s dialed %d times, want 1", id, seen[telnyx.CommandID(id)])
		}
	}
	if store.controlID["c1"] != "cc" || store.status["c1"] != "initiating" {
		t.Fatalf("c1 control=%q status=%s", store.controlID["c1"], store.status["c1"])
	}
}

func TestDialCarriesDeterministicCommandIDAndCallFields(t *testing.T) {
	store := newFakeStore(call("c1"))
	d := &fakeDialer{result: telnyx.DialResult{CallControlID: "cc"}}
	w := newWorker(t, store, fixedPlanner{dialer: d}, true)
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := d.dials[0]
	if p.CommandID != telnyx.CommandID("c1") {
		t.Fatalf("command id = %s", p.CommandID)
	}
	if p.To != "+14155550111" || p.TimeLimit != 600 {
		t.Fatalf("dial params = %+v", p)
	}
}

// Only a received 4xx answer proves a dial was refused. Every other failure may
// have placed a billable leg, so it is recorded as unconfirmed, and never
// redialed.
func TestDialFailureClassification(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		reason string
	}{
		{"received 4xx is rejected", &telnyx.APIError{Method: "POST", Path: "/calls", Status: 422}, ReasonDialRejected},
		{"5xx is unconfirmed", &telnyx.APIError{Method: "POST", Path: "/calls", Status: 503}, ReasonDialUnconfirmed},
		{"transport error is unconfirmed", errors.New("dial tcp: connection reset"), ReasonDialUnconfirmed},
		{"2xx with bad body is unconfirmed", fmt.Errorf("%w: dial response", telnyx.ErrBadResponse), ReasonDialUnconfirmed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore(call("c1"))
			d := &fakeDialer{err: tc.err}
			w := newWorker(t, store, fixedPlanner{dialer: d}, true)
			if err := w.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := w.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(d.dials) != 1 {
				t.Fatalf("dials = %d, want exactly 1", len(d.dials))
			}
			if store.status["c1"] != "failed" || store.reasons["c1"] != tc.reason {
				t.Fatalf("status=%s reason=%s, want failed/%s", store.status["c1"], store.reasons["c1"], tc.reason)
			}
		})
	}
}

func TestPlannerRefusalFailsWithoutDial(t *testing.T) {
	store := newFakeStore(call("c1"))
	d := &fakeDialer{}
	w := newWorker(t, store, fixedPlanner{dialer: d, reason: ReasonProfileUnusable}, true)
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.dials) != 0 {
		t.Fatal("refused plan still dialed")
	}
	if store.status["c1"] != "failed" || store.reasons["c1"] != ReasonProfileUnusable {
		t.Fatalf("status=%s reason=%s", store.status["c1"], store.reasons["c1"])
	}
}

func TestRecoverFailsUnconfirmedWithoutRedial(t *testing.T) {
	store := newFakeStore()
	store.status["old"] = "initiating"
	store.unconf = []string{"old"}
	d := &fakeDialer{}
	w := newWorker(t, store, fixedPlanner{dialer: d}, true)
	if err := w.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.status["old"] != "failed" || store.reasons["old"] != ReasonDialUnconfirmed {
		t.Fatalf("status=%s reason=%s", store.status["old"], store.reasons["old"])
	}
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(d.dials) != 0 {
		t.Fatal("recovered call was redialed")
	}
}

func TestCancelledContextStopsBeforeDial(t *testing.T) {
	store := newFakeStore(call("c1"))
	d := &fakeDialer{}
	w := newWorker(t, store, fixedPlanner{dialer: d}, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Tick(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(d.dials) != 0 {
		t.Fatal("dial after cancellation")
	}
}

func TestNewRequiresStoreAndPlanner(t *testing.T) {
	if _, err := New(nil, nil, Options{}); err == nil {
		t.Fatal("nil dependencies accepted")
	}
}
