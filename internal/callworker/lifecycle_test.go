package callworker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func runAsync(t *testing.T, w *Worker) (cancel func(), done chan error) {
	t.Helper()
	ctx, cancelCtx := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	return cancelCtx, done
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Run recovers first, then dials a call that is queued after it starts,
// because the wake from new work reaches the loop.
func TestRunDialsOnWake(t *testing.T) {
	store := newFakeStore()
	d := &fakeDialer{result: telnyx.DialResult{CallControlID: "cc-1"}}
	w, err := New(store, fixedPlanner{dialer: d}, Options{Enabled: true, Logger: quietLogger(), Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	cancel, done := runAsync(t, w)
	defer cancel()

	store.mu.Lock()
	store.queued = append(store.queued, call("c1"))
	store.status["c1"] = "queued"
	store.mu.Unlock()
	w.Wake()

	waitFor(t, "dial", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.dials) == 1
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run = %v", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.dials) != 1 {
		t.Fatalf("dials = %d, want exactly 1", len(d.dials))
	}
}

// Stop prevents new dials. A call queued before Stop is never sent.
func TestStopPreventsDial(t *testing.T) {
	store := newFakeStore(call("c1"))
	d := &fakeDialer{result: telnyx.DialResult{CallControlID: "cc-1"}}
	w, _ := New(store, fixedPlanner{dialer: d}, Options{Enabled: true, Logger: quietLogger(), Now: func() time.Time { return fixedNow }})
	w.Stop()
	w.Stop() // idempotent

	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()
	if err := <-done; err != nil {
		t.Fatalf("run = %v", err)
	}
	if len(d.dials) != 0 {
		t.Fatalf("stopped worker dialed %d times", len(d.dials))
	}
}

// A dial blocked when Abort runs is recorded as unconfirmed, never as
// rejected, because Telnyx may have accepted it. Nothing redials it.
type blockingDialer struct{ started chan struct{} }

func (b blockingDialer) Dial(ctx context.Context, _ telnyx.DialParams) (telnyx.DialResult, error) {
	close(b.started)
	<-ctx.Done()
	return telnyx.DialResult{}, ctx.Err()
}

func TestAbortedDialIsUnconfirmedAndNotRedialed(t *testing.T) {
	store := newFakeStore(call("c1"))
	started := make(chan struct{})
	w, _ := New(store, fixedPlanner{dialer: nil}, Options{Enabled: true, Logger: quietLogger(), Now: func() time.Time { return fixedNow }})
	w.planner = blockingPlanner{d: blockingDialer{started: started}}

	cancel, done := runAsync(t, w)
	defer cancel()
	<-started
	w.Stop()
	w.Abort()
	if err := <-done; err != nil {
		t.Fatalf("run = %v", err)
	}
	_ = cancel
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.status["c1"] != "failed" || store.reasons["c1"] != ReasonDialUnconfirmed {
		t.Fatalf("status=%q reason=%q, want failed/%s", store.status["c1"], store.reasons["c1"], ReasonDialUnconfirmed)
	}
}

type blockingPlanner struct{ d Dialer }

func (p blockingPlanner) Plan(context.Context, storage.QueuedCallRef) (Plan, string, error) {
	return Plan{Dialer: p.d, From: "+14155550100", ConnectionID: "conn-1",
		WebhookURL: "https://pub.example/hook", StreamURL: "wss://pub.example/stream", MaxSeconds: 600}, "", nil
}

// A plan error fails the claimed call instead of leaving it in initiating.
func TestPlanErrorFailsClaimedCall(t *testing.T) {
	store := newFakeStore(call("c1"))
	w, _ := New(store, fixedPlanner{err: context.DeadlineExceeded}, Options{Enabled: true, Logger: quietLogger(), Now: func() time.Time { return fixedNow }})
	_ = w.Tick(context.Background())
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.status["c1"] != "failed" || store.reasons["c1"] != ReasonPlanFailed {
		t.Fatalf("status=%q reason=%q", store.status["c1"], store.reasons["c1"])
	}
}

// hangRecorder records hangups. It is the Hanger a LegResolver returns.
type hangRecorder struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (h *hangRecorder) Hangup(_ context.Context, controlID, commandID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, controlID+"|"+commandID)
	return h.err
}

// legResolverFunc adapts a function to LegResolver.
type legResolverFunc func(context.Context, storage.LiveCallLeg) (Hanger, error)

func (f legResolverFunc) LegHanger(ctx context.Context, l storage.LiveCallLeg) (Hanger, error) {
	return f(ctx, l)
}

// HangupLive ends each live leg once with a command id derived from the call,
// and a failed leg does not stop the others.
func TestHangupLiveEndsEachLegOnceAndContinuesPastFailure(t *testing.T) {
	store := newFakeStore()
	store.status["a"], store.controlID["a"] = "active", "cc-a"
	store.status["b"], store.controlID["b"] = "ringing", "cc-b"
	store.status["done"], store.controlID["done"] = "ended", "cc-done"
	rec := &hangRecorder{err: errors.New("telnyx down")}
	w := newWorker(t, store, fixedPlanner{dialer: &fakeDialer{}}, true)
	res := legResolverFunc(func(context.Context, storage.LiveCallLeg) (Hanger, error) { return rec, nil })
	if err := w.HangupLive(context.Background(), res); err != nil {
		t.Fatalf("HangupLive = %v", err)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("hangups = %v, want one per live leg", rec.calls)
	}
	for _, c := range rec.calls {
		if !strings.Contains(c, "-hangup") {
			t.Fatalf("hangup command id not derived: %q", c)
		}
	}
}

// With the shutdown deadline already gone, no hangup is sent.
func TestHangupLiveStopsAtDeadline(t *testing.T) {
	store := newFakeStore()
	store.status["a"], store.controlID["a"] = "active", "cc-a"
	rec := &hangRecorder{}
	w := newWorker(t, store, fixedPlanner{dialer: &fakeDialer{}}, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := legResolverFunc(func(context.Context, storage.LiveCallLeg) (Hanger, error) { return rec, nil })
	if err := w.HangupLive(ctx, res); err == nil {
		t.Fatal("HangupLive past deadline returned nil")
	}
	if len(rec.calls) != 0 {
		t.Fatalf("hangup sent after deadline: %v", rec.calls)
	}
}
