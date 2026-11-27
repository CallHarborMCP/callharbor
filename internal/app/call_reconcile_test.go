package app

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/callworker"
	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// TestRestartActiveCallHangsUpOnceThenSignedHangupEnds covers a call that was
// active when the process crashed. After the restart it gets exactly one hangup
// to its recorded control id, with the owner's key, and does not redial. The
// signed call.hangup then ends it.
func TestRestartActiveCallHangsUpOnceThenSignedHangupEnds(t *testing.T) {
	const ctl = "v3:ctl-restart-active"
	tel := newFakeTelnyx(t, ctl)
	first := startCallApp(t, tel, nil)
	h := first.admin.headers(true)
	tpID := createTelephonyFixture(t, first, h)
	createAIFixture(t, first, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	first.stop()

	seedRestartRowsFor(t, first.dataDir, tpID, []restartRow{{"call_active", "active", "restart-active"}})
	setControlID(t, first.dataDir, "call_active", ctl)

	second := restartCallApp(t, tel, first.dataDir, first.pubKey, first.privKey)
	h2 := second.admin.headers(true)

	waitHangups(t, tel, 1)
	waitQuiet(tel)
	if got := tel.hangupsSeen(); len(got) != 1 || got[0] != ctl+"|"+callLegCommandID("call_active") {
		t.Fatalf("hangups after restart = %v, want exactly one to %s", got, ctl)
	}
	assertOneOwnerKeyHangup(t, tel, ctl, "call_active")
	if n := len(tel.dialBodies()); n != 0 {
		t.Fatalf("dials after restart = %d, want none (a live call is never redialed)", n)
	}
	if reason := callReason(t, second, h2, "call_active"); reason != "restart_reconciled" {
		t.Fatalf("outcome_reason_code = %q, want restart_reconciled", reason)
	}
	if got := waitStatusNow(t, second, h2, "call_active"); got != "ending" {
		t.Fatalf("status after restart = %q, want ending until the hangup webhook", got)
	}

	second.signedWebhook("call_active", "call.hangup", "evt-restart-active-hangup", ctl)
	waitForCallStatus(t, second, h2, "call_active", "ended")
}

// TestRestartQueuedCallDialsOnceAfterReconcile covers a queued call from before
// the restart alongside a live call. The live call is hung up and the queued one
// is dialed exactly once.
func TestRestartQueuedCallDialsOnceAfterReconcile(t *testing.T) {
	const ctl = "v3:ctl-restart-mixed"
	tel := newFakeTelnyx(t, ctl)
	first := startCallApp(t, tel, nil)
	h := first.admin.headers(true)
	tpID := createTelephonyFixture(t, first, h)
	createAIFixture(t, first, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	first.stop()

	seedRestartRowsFor(t, first.dataDir, tpID, []restartRow{
		{"call_ringing", "ringing", "restart-ringing"},
		{"call_queued_pre", "queued", "restart-queued-pre"},
	})
	setControlID(t, first.dataDir, "call_ringing", "v3:ringing-pre")

	restartCallApp(t, tel, first.dataDir, first.pubKey, first.privKey)
	tel.waitDials(t, 1)
	waitHangups(t, tel, 1)
	waitQuiet(tel)

	if n := len(tel.dialBodies()); n != 1 {
		t.Fatalf("dials after restart = %d, want exactly one (the queued call)", n)
	}
	if got := tel.hangupsSeen(); len(got) != 1 {
		t.Fatalf("hangups after restart = %v, want exactly one (the ringing call)", got)
	}
}

// TestReconciledCallFreesConcurrencySlot covers the concurrency limit. With a
// per-user limit of 1, a live call holds the slot and a second MakeCall is
// refused with concurrency_limit. Once the restart reconciles the call and its
// leg ends, the same MakeCall is accepted.
func TestReconciledCallFreesConcurrencySlot(t *testing.T) {
	const ctl = "v3:ctl-slot"
	tel := newFakeTelnyx(t, ctl)
	first := startCallAppWith(t, tel, nil, func(c *config.Config) {
		c.CallPolicy = config.DefaultCallPolicy()
		c.CallPolicy.UserMaxConcurrentCalls = 1
		c.CallPolicy.UserMaxCallsPerHour = 1000
	})
	h := first.admin.headers(true)
	tpID := createTelephonyFixture(t, first, h)
	createAIFixture(t, first, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	first.stop()

	seedRestartRowsFor(t, first.dataDir, tpID, []restartRow{{"call_holds_slot", "active", "restart-slot"}})
	setControlID(t, first.dataDir, "call_holds_slot", ctl)

	// Before the restart reconciles the call, the slot is held: a second call is
	// refused. The second run holds the call as ending, which still counts.
	second := restartCallAppWith(t, tel, first.dataDir, first.pubKey, first.privKey, func(c *config.Config) {
		c.CallPolicy = config.DefaultCallPolicy()
		c.CallPolicy.UserMaxConcurrentCalls = 1
		c.CallPolicy.UserMaxCallsPerHour = 1000
	})
	h2 := second.admin.headers(true)
	waitHangups(t, tel, 1)

	status, _, body := sendJSON(t, second.client, http.MethodPost, second.base+"/api/v1/calls",
		policyCallBody(tpID, "+14155550111"), withIdempotency(h2, "slot-refused"))
	if status != http.StatusConflict || !containsReason(string(body), "concurrency_limit") {
		t.Fatalf("second call while the slot is held = %d %s, want 409 concurrency_limit", status, body)
	}

	// The leg ends at Telnyx and the call's hangup webhook arrives. The slot
	// then frees, so the same request is accepted.
	second.signedWebhook("call_holds_slot", "call.hangup", "evt-slot-hangup", ctl)
	waitForCallStatus(t, second, h2, "call_holds_slot", "ended")

	status, _, body = sendJSON(t, second.client, http.MethodPost, second.base+"/api/v1/calls",
		policyCallBody(tpID, "+14155550111"), withIdempotency(h2, "slot-accepted"))
	if status != http.StatusAccepted {
		t.Fatalf("call after the slot freed = %d %s, want 202", status, body)
	}
}

// setControlID records the provider control id of a seeded call, as a dial
// would have. Restart recovery only ends a leg it can hang up, so the seeded
// live call needs one.
func setControlID(t *testing.T, dataDir, callID, controlID string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, databaseFileName)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE calls SET provider_call_control_id = ? WHERE id = ?`, controlID, callID); err != nil {
		t.Fatalf("set control id for %s: %v", callID, err)
	}
}

// seedEndingHangupSent marks an ending call's last hangup as sent at the given
// time with the given attempt count, as an earlier hangup send would have. The
// sweep reads these to decide when the leg is due for a status check.
func seedEndingHangupSent(t *testing.T, dataDir, callID string, sentAt time.Time, attempts int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, databaseFileName)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	// The store writes timestamps as fixed-width milliseconds in UTC. The
	// seed uses the same format, so the sweep's comparison reads it as it
	// would a real stamp.
	if _, err := db.Exec(`UPDATE calls SET hangup_requested_at = ?, hangup_attempts = ? WHERE id = ?`,
		sentAt.UTC().Format("2006-01-02T15:04:05.000Z"), attempts, callID); err != nil {
		t.Fatalf("seed hangup marker for %s: %v", callID, err)
	}
}

// setOutcomeReason records the stored reason of a seeded call. The sweep must
// keep it when it ends the leg.
func setOutcomeReason(t *testing.T, dataDir, callID, reason string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, databaseFileName)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE calls SET outcome_reason_code = ? WHERE id = ?`, reason, callID); err != nil {
		t.Fatalf("set reason for %s: %v", callID, err)
	}
}

// callLegCommandID is the command id the leg hangup uses for a call. Tests
// compare against it so the assertion is the production derivation.
func callLegCommandID(callID string) string {
	return callworker.LegCommandID(callID)
}

// containsReason reports whether a JSON error body carries the stable reason
// code. The API writes it as the leading token of the error message.
func containsReason(body, reason string) bool {
	return strings.Contains(body, `"message":"`+reason+`:`)
}

// endingSweepCase is one status answer for an ending leg whose hangup was never
// confirmed by a call.hangup webhook.
type endingSweepCase struct {
	name  string
	alive *bool // nil leaves the status unscripted (alive)
	gone  bool  // true makes the status read answer 404
	want  string
}

// TestEndingSweepUsesOwnerKeyAndStatusAnswer drives the reconciliation sweep
// through the real app. A call is left in ending with a hangup sent over a
// minute ago and no webhook. A fake clock passes the retry interval, so the
// sweep reads the leg's status with the owner's key. The answer decides the
// outcome:
//   - is_alive=false, or 404 (not alive, live-unverified): the call ends with its
//     reason kept, and a call.reconciled event is recorded.
//   - is_alive=true: the same idempotent hangup is resent, with the same
//     command id, and the call stays ending.
func TestEndingSweepUsesOwnerKeyAndStatusAnswer(t *testing.T) {
	no, yes := false, true
	cases := []endingSweepCase{
		{name: "not_alive_ends", alive: &no, want: "ended"},
		{name: "not_found_ends", gone: true, want: "ended"},
		{name: "alive_resends_hangup", alive: &yes, want: "ending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctl := "v3:ctl-ending-" + tc.name
			tel := newFakeTelnyx(t, ctl)
			first := startCallApp(t, tel, nil)
			h := first.admin.headers(true)
			tpID := createTelephonyFixture(t, first, h)
			createAIFixture(t, first, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
			first.stop()

			seedRestartRowsFor(t, first.dataDir, tpID, []restartRow{{"call_ending", "ending", "restart-ending-" + tc.name}})
			setControlID(t, first.dataDir, "call_ending", ctl)
			setOutcomeReason(t, first.dataDir, "call_ending", "hangup_requested")
			// The clock starts at t0. The restart sends one hangup to the ending
			// leg at t0, as recovery does, and stamps the marker. Then the clock
			// moves 61 s on, past the retry interval, and the next worker tick
			// runs the sweep.
			t0 := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
			seedEndingHangupSent(t, first.dataDir, "call_ending", t0.Add(-time.Hour), 1)

			if tc.alive != nil {
				tel.setCallAlive(ctl, *tc.alive)
			}
			if tc.gone {
				tel.setCallGone(ctl)
			}

			clk := &testClock{now: t0}
			second := restartCallAppAt(t, tel, first.dataDir, first.pubKey, first.privKey, nil, clk.Now)
			h2 := second.admin.headers(true)

			// The restart hangup is the first send to this leg.
			waitHangups(t, tel, 1)
			clk.Set(t0.Add(61 * time.Second))

			// The status read is the sweep's first network call on this leg.
			waitStatusRead(t, tel, ctl)
			if tc.want == "ended" {
				waitForCallStatus(t, second, h2, "call_ending", "ended")
				if reason := callReason(t, second, h2, "call_ending"); reason != "hangup_requested" {
					t.Fatalf("outcome_reason_code = %q, want the seeded hangup_requested kept", reason)
				}
				if n := countCallEvents(t, first.dataDir, "call_ending", "call.reconciled"); n != 1 {
					t.Fatalf("call.reconciled events = %d, want exactly one", n)
				}
				waitQuiet(tel)
				if got := tel.hangupsSeen(); len(got) != 1 {
					t.Fatalf("hangups = %v, want only the restart send (not alive needs no resend)", got)
				}
			} else {
				// Still ending. The sweep resends the hangup with the owner's key
				// and the same command id the restart used.
				waitHangups(t, tel, 2)
				waitQuiet(tel)
				got := tel.hangupsSeen()
				want := ctl + "|" + legHangupCommandID("call_ending")
				if len(got) != 2 || got[0] != want || got[1] != want {
					t.Fatalf("hangups = %v, want the restart send and one resend, both %s", got, want)
				}
				if got := waitStatusNow(t, second, h2, "call_ending"); got != "ending" {
					t.Fatalf("status after alive answer = %q, want ending", got)
				}
			}
			reads := tel.statusReadsSeen()
			if len(reads) == 0 || reads[0] != ctl+"|"+ownerKey {
				t.Fatalf("status reads = %v, want the leg read with the owner's key", reads)
			}
		})
	}
}

// testClock is a clock a test moves forward explicitly. The worker reads it
// on each tick, so no test waits on the wall clock for a deadline.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

// Now returns the clock's current time.
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Set moves the clock to t.
func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// waitStatusRead waits until the fake Telnyx has answered one status read for
// the control id. The worker ticks every 5 s, so the wait covers a full tick.
func waitStatusRead(t *testing.T, tel *fakeTelnyx, ctl string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range tel.statusReadsSeen() {
			if strings.HasPrefix(r, ctl+"|") {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no status read for %s reached the fake Telnyx", ctl)
}

// countCallEvents counts the call_events rows of one kind for a call.
func countCallEvents(t *testing.T, dataDir, callID, kind string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, databaseFileName)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM call_events WHERE call_id = ? AND kind = ?`, callID, kind).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", kind, err)
	}
	return n
}
