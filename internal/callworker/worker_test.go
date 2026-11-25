package callworker

import (
	"context"
	"errors"
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
	mu        sync.Mutex
	queued    []storage.QueuedCallRef
	status    map[string]string
	controlID map[string]string
	reasons   map[string]string
	unconf    []string
}

func newFakeStore(calls ...storage.QueuedCallRef) *fakeStore {
	s := &fakeStore{status: map[string]string{}, controlID: map[string]string{}, reasons: map[string]string{}}
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
		if cid := s.controlID[id]; cid != "" && (st == "ringing" || st == "active") {
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

func (s *fakeStore) RecordProviderCallControlID(_ context.Context, id, _, control string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status[id] != "initiating" {
		return errors.New("not initiating")
	}
	s.controlID[id] = control
	return nil
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

func TestDialErrorFailsCallWithoutRedial(t *testing.T) {
	store := newFakeStore(call("c1"))
	d := &fakeDialer{err: errors.New("telnyx: refused")}
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
	if store.status["c1"] != "failed" || store.reasons["c1"] != ReasonDialRejected {
		t.Fatalf("status=%s reason=%s", store.status["c1"], store.reasons["c1"])
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
