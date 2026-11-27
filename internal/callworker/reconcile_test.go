package callworker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

// scriptHanger records hangups and answers GetCallStatus from a script. It is
// the provider stand-in for reconciliation tests, which never use a real clock.
type scriptHanger struct {
	mu       sync.Mutex
	controls []string
	commands []string
	status   telnyx.CallStatus
	statErr  error
	hangErr  error
	checks   int
}

func (h *scriptHanger) Hangup(_ context.Context, control, command string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.controls = append(h.controls, control)
	h.commands = append(h.commands, command)
	return h.hangErr
}

func (h *scriptHanger) GetCallStatus(_ context.Context, control string) (telnyx.CallStatus, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checks++
	if h.statErr != nil {
		return telnyx.CallStatus{}, h.statErr
	}
	st := h.status
	st.CallControlID = control
	return st, nil
}

func (h *scriptHanger) hangups() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.controls)
}

// hangerLegs returns the same hanger for every leg.
type hangerLegs struct{ h Hanger }

func (l hangerLegs) LegHanger(context.Context, storage.LiveCallLeg) (Hanger, error) {
	return l.h, nil
}

// reconStore serves scripted reconciliation lists over the shared fakeStore,
// which holds the status and marker state the assertions read.
type reconStore struct {
	*fakeStore
	ending  []storage.ReconcileCall
	overdue []storage.ReconcileCall
	orphans []storage.ReconcileCall
}

func (s *reconStore) ListEndingForReconcile(context.Context, time.Time, int) ([]storage.ReconcileCall, error) {
	return s.ending, nil
}

func (s *reconStore) ListOverdueCalls(context.Context, time.Time, int, int) ([]storage.ReconcileCall, error) {
	return s.overdue, nil
}

func (s *reconStore) ListOrphanedTerminal(context.Context, storage.OrphanBounds, int) ([]storage.ReconcileCall, error) {
	return s.orphans, nil
}

// reconCall is a call row as the sweep reads it, created at fixedNow.
func reconCall(id, status, control string) storage.ReconcileCall {
	return storage.ReconcileCall{ID: id, OwnerUserID: "owner", TelephonyProfileID: "tp",
		TelephonyOwnerUserID: "owner", ControlID: control, Status: status,
		MaxDurationSeconds: 600, CreatedAt: fixedNow.Add(-2 * time.Minute), LastHangupAt: fixedNow.Add(-2 * time.Minute)}
}

// newReconWorker builds a worker whose reconciliation uses the given store and
// hanger, with the clock held at fixedNow.
func newReconWorker(t *testing.T, store Store, h Hanger) *Worker {
	t.Helper()
	w, err := New(store, fixedPlanner{}, Options{Enabled: true, Now: func() time.Time { return fixedNow }, Legs: hangerLegs{h: h}})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// setStatus puts a call in a status in the fake store, with a control id.
func setStatus(s *fakeStore, id, status, control string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[id] = status
	if control != "" {
		s.controlID[id] = control
	}
}

// TestRecoverHangsUpLiveLegOnceWithoutRedial covers a ringing call that was live
// before the restart. It ends with restart_reconciled, gets one hangup to its
// recorded control id with the stable command id, and its attempt is recorded.
func TestRecoverHangsUpLiveLegOnceWithoutRedial(t *testing.T) {
	store := newFakeStore()
	setStatus(store, "c-live", "ringing", "v3:live")
	h := &scriptHanger{}
	w := newReconWorker(t, store, h)

	if err := w.Recover(context.Background()); err != nil {
		t.Fatalf("Recover = %v", err)
	}
	if got := h.hangups(); got != 1 {
		t.Fatalf("hangups = %d, want exactly 1", got)
	}
	if h.controls[0] != "v3:live" || h.commands[0] != LegCommandID("c-live") {
		t.Fatalf("hangup = (%q, %q), want the recorded control id and stable command id", h.controls[0], h.commands[0])
	}
	if store.status["c-live"] != "ending" || store.reasons["c-live"] != ReasonRestartReconciled {
		t.Fatalf("status=%q reason=%q, want ending/%s", store.status["c-live"], store.reasons["c-live"], ReasonRestartReconciled)
	}
	if store.hangupSent["c-live"] != 1 {
		t.Fatalf("recorded attempts = %d, want 1", store.hangupSent["c-live"])
	}
}

// TestRecoverKeepsEndingReasonAndHangsUpOnce covers a call already ending before
// the restart. The reason it had is kept, and it still gets exactly one hangup.
func TestRecoverKeepsEndingReasonAndHangsUpOnce(t *testing.T) {
	store := newFakeStore()
	setStatus(store, "c-ending", "ending", "v3:ending")
	store.reasons["c-ending"] = "hangup_requested"
	h := &scriptHanger{}
	w := newReconWorker(t, store, h)

	if err := w.Recover(context.Background()); err != nil {
		t.Fatalf("Recover = %v", err)
	}
	if got := h.hangups(); got != 1 {
		t.Fatalf("hangups = %d, want exactly 1", got)
	}
	if store.reasons["c-ending"] != "hangup_requested" {
		t.Fatalf("reason = %q, want the existing reason kept", store.reasons["c-ending"])
	}
}

// TestRecoverNeverRedialsButDialsQueuedOnce covers the restart invariant. An
// unconfirmed initiating call fails and is never dialed. A queued call from
// before the restart dials exactly once.
func TestRecoverNeverRedialsButDialsQueuedOnce(t *testing.T) {
	store := newFakeStore(call("c-queued"))
	store.status["c-unconf"] = "initiating"
	store.unconf = []string{"c-unconf"}
	dialer := &fakeDialer{result: telnyx.DialResult{CallControlID: "v3:q"}}
	h := &scriptHanger{}
	w, err := New(store, fixedPlanner{dialer: dialer}, Options{Enabled: true, Now: func() time.Time { return fixedNow }, Legs: hangerLegs{h: h}})
	if err != nil {
		t.Fatal(err)
	}

	if err := w.Recover(context.Background()); err != nil {
		t.Fatalf("Recover = %v", err)
	}
	if err := w.Tick(context.Background()); err != nil {
		t.Fatalf("Tick = %v", err)
	}
	if store.status["c-unconf"] != "failed" || store.reasons["c-unconf"] != ReasonDialUnconfirmed {
		t.Fatalf("unconfirmed status=%q reason=%q, want failed/%s", store.status["c-unconf"], store.reasons["c-unconf"], ReasonDialUnconfirmed)
	}
	if n := len(dialer.dials); n != 1 {
		t.Fatalf("dials = %d, want exactly one (the queued call only)", n)
	}
	if dialer.dials[0].ClientState == nil || string(dialer.dials[0].ClientState) != "c-queued" {
		t.Fatalf("dialed call = %q, want c-queued", dialer.dials[0].ClientState)
	}
}

// TestReconcileEndingNotAliveEndsWithoutHangup covers an ending leg Telnyx says
// is gone. It is ended without a resend.
func TestReconcileEndingNotAliveEndsWithoutHangup(t *testing.T) {
	fs := newFakeStore()
	setStatus(fs, "c-gone", "ending", "v3:gone")
	h := &scriptHanger{status: telnyx.CallStatus{IsAlive: false}}
	store := &reconStore{fakeStore: fs, ending: []storage.ReconcileCall{reconCall("c-gone", "ending", "v3:gone")}}
	w := newReconWorker(t, store, h)

	if err := w.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	if fs.status["c-gone"] != "ended" {
		t.Fatalf("status = %q, want ended", fs.status["c-gone"])
	}
	if h.hangups() != 0 {
		t.Fatalf("hangups = %d, want none for a leg Telnyx reports gone", h.hangups())
	}
}

// TestReconcileEndingNotFoundAndUnprocessableCountAsNotAlive covers 404 and 422
// from the status read. Both are treated as not alive, so the call ends.
func TestReconcileEndingNotFoundAndUnprocessableCountAsNotAlive(t *testing.T) {
	cases := map[string]error{
		"404": telnyx.ErrNotFound,
		"422": &telnyx.APIError{Status: 422},
	}
	for name, statErr := range cases {
		t.Run(name, func(t *testing.T) {
			fs := newFakeStore()
			setStatus(fs, "c-x", "ending", "v3:x")
			h := &scriptHanger{statErr: statErr}
			store := &reconStore{fakeStore: fs, ending: []storage.ReconcileCall{reconCall("c-x", "ending", "v3:x")}}
			w := newReconWorker(t, store, h)
			if err := w.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile = %v", err)
			}
			if fs.status["c-x"] != "ended" || h.hangups() != 0 {
				t.Fatalf("status=%q hangups=%d, want ended and no resend", fs.status["c-x"], h.hangups())
			}
		})
	}
}

// TestReconcileEndingAliveResendsSameHangupAndBumpsAttempts covers a leg Telnyx
// still reports alive. The same hangup is resent, and the attempt is recorded.
func TestReconcileEndingAliveResendsSameHangupAndBumpsAttempts(t *testing.T) {
	fs := newFakeStore()
	setStatus(fs, "c-alive", "ending", "v3:alive")
	h := &scriptHanger{status: telnyx.CallStatus{IsAlive: true}}
	store := &reconStore{fakeStore: fs, ending: []storage.ReconcileCall{reconCall("c-alive", "ending", "v3:alive")}}
	w := newReconWorker(t, store, h)

	if err := w.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	if h.hangups() != 1 || h.commands[0] != LegCommandID("c-alive") || h.controls[0] != "v3:alive" {
		t.Fatalf("hangups=%v commands=%v, want one resend with the same command id", h.controls, h.commands)
	}
	if fs.status["c-alive"] != "ending" || fs.hangupSent["c-alive"] != 1 {
		t.Fatalf("status=%q attempts=%d, want ending with one recorded attempt", fs.status["c-alive"], fs.hangupSent["c-alive"])
	}
}

// TestReconcileTransportErrorResendsAndCountsAttempt covers a status read that
// fails in transport or with a 5xx. Those do not prove the leg is gone, so the
// hangup is resent rather than the call being ended.
func TestReconcileTransportErrorResendsAndCountsAttempt(t *testing.T) {
	fs := newFakeStore()
	setStatus(fs, "c-5xx", "ending", "v3:5xx")
	h := &scriptHanger{statErr: &telnyx.APIError{Status: 503}}
	store := &reconStore{fakeStore: fs, ending: []storage.ReconcileCall{reconCall("c-5xx", "ending", "v3:5xx")}}
	w := newReconWorker(t, store, h)

	if err := w.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	if fs.status["c-5xx"] != "ending" {
		t.Fatalf("status = %q, want still ending on a 5xx", fs.status["c-5xx"])
	}
	if h.hangups() != 1 {
		t.Fatalf("hangups = %d, want one resend", h.hangups())
	}
}

// TestReconcileExpiryBoundRequiresAttemptsAndTimeLimit covers the expiry bound.
// Retries continue until the bound: max attempts are used and the Telnyx time
// limit has passed. Before that, a resend is sent and the call stays ending.
func TestReconcileExpiryBoundRequiresAttemptsAndTimeLimit(t *testing.T) {
	t.Run("max attempts but limit not passed keeps retrying", func(t *testing.T) {
		fs := newFakeStore()
		setStatus(fs, "c-early", "ending", "v3:early")
		h := &scriptHanger{statErr: &telnyx.APIError{Status: 500}}
		c := reconCall("c-early", "ending", "v3:early")
		c.Attempts = ReconcileMaxAttempts
		store := &reconStore{fakeStore: fs, ending: []storage.ReconcileCall{c}}
		w := newReconWorker(t, store, h)
		if err := w.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile = %v", err)
		}
		if fs.status["c-early"] != "ending" || h.hangups() != 1 {
			t.Fatalf("status=%q hangups=%d, want ending with one more resend", fs.status["c-early"], h.hangups())
		}
	})
	t.Run("max attempts and limit passed expires", func(t *testing.T) {
		fs := newFakeStore()
		setStatus(fs, "c-expired", "ending", "v3:expired")
		h := &scriptHanger{statErr: &telnyx.APIError{Status: 500}}
		c := reconCall("c-expired", "ending", "v3:expired")
		c.Attempts = ReconcileMaxAttempts
		c.CreatedAt = fixedNow.Add(-time.Hour)
		store := &reconStore{fakeStore: fs, ending: []storage.ReconcileCall{c}}
		w := newReconWorker(t, store, h)
		if err := w.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile = %v", err)
		}
		if fs.status["c-expired"] != "ended" || fs.reasons["c-expired"] != ReasonReconcileExpired {
			t.Fatalf("status=%q reason=%q, want ended/%s", fs.status["c-expired"], fs.reasons["c-expired"], ReasonReconcileExpired)
		}
		if h.hangups() != 0 {
			t.Fatalf("hangups = %d, want none once expired", h.hangups())
		}
	})
}

// TestReconcileOverdueRingingEndsWithMaxDurationAndOneHangup covers the
// watchdog. A ringing call past its deadline is ended once with max_duration,
// with a single hangup to its control id.
func TestReconcileOverdueRingingEndsWithMaxDurationAndOneHangup(t *testing.T) {
	fs := newFakeStore()
	setStatus(fs, "c-over", "ringing", "v3:over")
	h := &scriptHanger{}
	c := reconCall("c-over", "ringing", "v3:over")
	c.CreatedAt = fixedNow.Add(-time.Hour)
	store := &reconStore{fakeStore: fs, overdue: []storage.ReconcileCall{c}}
	w := newReconWorker(t, store, h)

	if err := w.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	if fs.reasons["c-over"] != ReasonMaxDuration || fs.status["c-over"] != "ending" {
		t.Fatalf("status=%q reason=%q, want ending/%s", fs.status["c-over"], fs.reasons["c-over"], ReasonMaxDuration)
	}
	if h.hangups() != 1 || h.controls[0] != "v3:over" {
		t.Fatalf("hangups=%v, want one to v3:over", h.controls)
	}
}

// TestReconcileOverdueInitiatingEndsWithReconcileTimeout covers an initiating
// call whose control id was recorded but whose webhooks never came.
func TestReconcileOverdueInitiatingEndsWithReconcileTimeout(t *testing.T) {
	fs := newFakeStore()
	setStatus(fs, "c-init", "initiating", "v3:init")
	h := &scriptHanger{}
	store := &reconStore{fakeStore: fs, overdue: []storage.ReconcileCall{reconCall("c-init", "initiating", "v3:init")}}
	w := newReconWorker(t, store, h)

	if err := w.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	if fs.reasons["c-init"] != ReasonReconcileTimeout || h.hangups() != 1 {
		t.Fatalf("reason=%q hangups=%d, want %s and one hangup", fs.reasons["c-init"], h.hangups(), ReasonReconcileTimeout)
	}
}

// TestReconcileOrphanedTerminalSendsOneRecordedHangup covers a terminal call
// with a control id and no hangup seen. One hangup is sent and recorded.
func TestReconcileOrphanedTerminalSendsOneRecordedHangup(t *testing.T) {
	fs := newFakeStore()
	fs.status["c-orphan"] = "failed"
	fs.controlID["c-orphan"] = "v3:orphan"
	h := &scriptHanger{}
	store := &reconStore{fakeStore: fs, orphans: []storage.ReconcileCall{reconCall("c-orphan", "failed", "v3:orphan")}}
	w := newReconWorker(t, store, h)

	if err := w.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	if h.hangups() != 1 || h.controls[0] != "v3:orphan" || fs.hangupSent["c-orphan"] != 1 {
		t.Fatalf("hangups=%v attempts=%d, want one hangup and one recorded attempt", h.controls, fs.hangupSent["c-orphan"])
	}
}

// TestReconcileUsesBatchSizeFromOptions covers the batch bound. The ending
// query is asked for exactly BatchSize calls, so one pass is bounded.
func TestReconcileUsesBatchSizeFromOptions(t *testing.T) {
	var asked int
	fs := newFakeStore()
	store := &sizedStore{reconStore: reconStore{fakeStore: fs}, asked: &asked}
	w, err := New(store, fixedPlanner{}, Options{Enabled: true, BatchSize: 3, Now: func() time.Time { return fixedNow }, Legs: hangerLegs{h: &scriptHanger{}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	if asked != 3 {
		t.Fatalf("batch limit = %d, want 3 for every list", asked)
	}
}

// sizedStore records the limit passed to each list query.
type sizedStore struct {
	reconStore
	asked *int
}

func (s *sizedStore) ListEndingForReconcile(_ context.Context, _ time.Time, limit int) ([]storage.ReconcileCall, error) {
	*s.asked = limit
	return nil, nil
}
