package callworker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/outcome"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// outcomeFake adds the outcome store methods to the worker's fake store. Ready
// calls are listed until one outcome is recorded; a recorded call is never
// listed again, as storage's readiness query behaves.
type outcomeFake struct {
	*fakeStore
	mu        sync.Mutex
	ready     map[string]bool
	outcomes  map[string]outcome.Result
	loads     int
	records   int
	loadErr   error
	recordErr error
}

func newOutcomeFake(ready ...string) *outcomeFake {
	f := &outcomeFake{fakeStore: newFakeStore(), ready: map[string]bool{}, outcomes: map[string]outcome.Result{}}
	for _, id := range ready {
		f.ready[id] = true
	}
	return f
}

func (f *outcomeFake) ListCallsAwaitingOutcome(_ context.Context, _ time.Time, _ time.Duration, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id := range f.ready {
		if _, done := f.outcomes[id]; !done && len(out) < limit {
			out = append(out, id)
		}
	}
	return out, nil
}

func (f *outcomeFake) LoadOutcomeFacts(_ context.Context, callID string) (storage.OutcomeFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.loadErr != nil {
		return storage.OutcomeFacts{}, f.loadErr
	}
	// An answered realtime call with no assessment: the evaluator returns unknown.
	return storage.OutcomeFacts{
		CallID: callID, Status: "ended", Protocol: outcome.ProtocolRealtimeWS, Answered: true,
		TranscriptStarted: true, TranscriptFinalized: true,
	}, nil
}

func (f *outcomeFake) RecordTaskOutcome(_ context.Context, callID string, r outcome.Result, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return false, f.recordErr
	}
	if _, done := f.outcomes[callID]; done {
		return false, nil
	}
	f.outcomes[callID] = r
	f.records++
	return true, nil
}

func newOutcomeWorker(t *testing.T, store Store, d *fakeDialer, logger *slog.Logger) *Worker {
	t.Helper()
	w, err := New(store, fixedPlanner{dialer: d}, Options{Enabled: true, Now: func() time.Time { return fixedNow }, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// A ready call is evaluated once. A second tick finds nothing to evaluate and
// leaves the recorded outcome exactly as it was.
func TestReadyCallEvaluatedOnceAndIdempotent(t *testing.T) {
	store := newOutcomeFake("c1")
	d := &fakeDialer{result: telnyx.DialResult{CallControlID: "cc"}}
	w := newOutcomeWorker(t, store, d, nil)

	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := store.outcomes["c1"]
	if first.Outcome != outcome.Unknown || store.records != 1 {
		t.Fatalf("after first tick: outcome=%+v records=%d", first, store.records)
	}
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.records != 1 || store.loads != 1 {
		t.Fatalf("second tick re-evaluated: records=%d loads=%d", store.records, store.loads)
	}
	if store.outcomes["c1"].Outcome != first.Outcome {
		t.Fatalf("outcome changed: %+v -> %+v", first, store.outcomes["c1"])
	}
}

// A call that is not ready is never loaded or recorded.
func TestNotReadyCallIsSkipped(t *testing.T) {
	store := newOutcomeFake()
	d := &fakeDialer{result: telnyx.DialResult{CallControlID: "cc"}}
	w := newOutcomeWorker(t, store, d, nil)

	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.loads != 0 || store.records != 0 {
		t.Fatalf("not-ready call evaluated: loads=%d records=%d", store.loads, store.records)
	}
}

// A store failure is logged with the call id and the error only. It does not
// stop dialing a queued call in the same tick.
func TestOutcomeStoreErrorLoggedAndDialingContinues(t *testing.T) {
	store := &outcomeFake{fakeStore: newFakeStore(call("c1")), ready: map[string]bool{"c9": true},
		outcomes: map[string]outcome.Result{}, loadErr: errors.New("database is locked")}
	d := &fakeDialer{result: telnyx.DialResult{CallControlID: "cc"}}

	var buf bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, buf: &buf}, nil))
	w := newOutcomeWorker(t, store, d, logger)

	err := w.Tick(context.Background())
	if err == nil {
		t.Fatal("Tick returned nil; the outcome failure should be reported to the caller")
	}
	if len(d.dials) != 1 {
		t.Fatalf("dials = %d, want 1: an outcome error must not block dialing", len(d.dials))
	}
	mu.Lock()
	logged := buf.String()
	mu.Unlock()
	if !strings.Contains(logged, "c9") || !strings.Contains(logged, "database is locked") {
		t.Fatalf("log = %q, want the call id and the error", logged)
	}
	// Only the call id and the error text may be logged. The log record has no
	// other attributes, so no brief, quote or transcript can appear in it.
	if strings.Count(logged, "call_id=") != 1 || !strings.Contains(logged, "error=") {
		t.Fatalf("log record shape = %q, want exactly call_id and error", logged)
	}
}

// lockedWriter serialises log writes for the test buffer.
type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
