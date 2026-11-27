package callworker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// recordingHanger records every hangup it is asked to send.
type recordingHanger struct {
	mu       sync.Mutex
	controls []string
	commands []string
}

func (h *recordingHanger) Hangup(_ context.Context, control, command string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.controls = append(h.controls, control)
	h.commands = append(h.commands, command)
	return nil
}

func (h *recordingHanger) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.controls)
}

// legsFor returns one hanger for every leg, so a test can count hangups.
type legsFor struct{ h *recordingHanger }

func (l legsFor) LegHanger(context.Context, storage.LiveCallLeg) (Hanger, error) {
	return l.h, nil
}

// endingDialer is a dialer whose dial returns after the call has been moved to
// ending by a hangup. It is the in-flight dial case the spec requires.
type endingDialer struct {
	store  *fakeStore
	id     string
	result telnyx.DialResult
}

func (d endingDialer) Dial(_ context.Context, _ telnyx.DialParams) (telnyx.DialResult, error) {
	// A hangup arrives while the dial is in flight. The status moves to ending
	// without a control id, as RequestHangup does during a dial.
	d.store.mu.Lock()
	d.store.status[d.id] = "ending"
	d.store.mu.Unlock()
	return d.result, nil
}

// TestHangupDuringDialSendsExactlyOneHangupToReturnedControlID covers the
// in-flight dial case: the dial returns a control id for a call that became
// ending, and the worker sends one hangup to that id with the stable command id.
func TestHangupDuringDialSendsExactlyOneHangupToReturnedControlID(t *testing.T) {
	store := newFakeStore(call("c-inflight"))
	h := &recordingHanger{}
	dialer := endingDialer{store: store, id: "c-inflight", result: telnyx.DialResult{CallControlID: "v3:inflight"}}
	w, err := New(store, plannerWith(dialer), Options{Enabled: true, Now: func() time.Time { return fixedNow }, Legs: legsFor{h: h}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick = %v", err)
	}
	if got := h.count(); got != 1 {
		t.Fatalf("hangups = %d, want exactly 1", got)
	}
	if h.controls[0] != "v3:inflight" {
		t.Fatalf("hangup control = %q, want the returned control id", h.controls[0])
	}
	if h.commands[0] != LegCommandID("c-inflight") {
		t.Fatalf("hangup command = %q, want the stable leg command id", h.commands[0])
	}
}

// TestNoHangupWhenDialReturnsForActiveCall shows that a normal dial, with no
// hangup requested, sends no hangup.
func TestNoHangupWhenDialReturnsForActiveCall(t *testing.T) {
	store := newFakeStore(call("c-normal"))
	h := &recordingHanger{}
	dialer := &fakeDialer{result: telnyx.DialResult{CallControlID: "v3:normal"}}
	w, err := New(store, fixedPlanner{dialer: dialer}, Options{Enabled: true, Now: func() time.Time { return fixedNow }, Legs: legsFor{h: h}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick = %v", err)
	}
	if got := h.count(); got != 0 {
		t.Fatalf("hangups = %d, want 0", got)
	}
}

// plannerWith adapts a dialer that changes state into a planner.
func plannerWith(d Dialer) Planner {
	return dialerPlanner{d: d}
}

type dialerPlanner struct{ d Dialer }

func (p dialerPlanner) Plan(context.Context, storage.QueuedCallRef) (Plan, string, error) {
	return Plan{Dialer: p.d, From: "+14155550100", ConnectionID: "conn-1",
		WebhookURL: "https://pub.example/hook", StreamURL: "wss://pub.example/stream", MaxSeconds: 600}, "", nil
}
