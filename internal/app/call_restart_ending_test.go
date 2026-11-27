package app

import (
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// TestRestartAfterHangupDuringDialReleasesSlotWithoutRedial covers review B1. A
// dial is in flight and the caller hangs up, so the call is ending with no
// provider control id. The process then crashes before Telnyx's response is
// recorded. On restart the call must be failed with dial_unconfirmed, which
// releases its concurrency slot, so a call the slot blocked is accepted. No
// dial may be repeated.
//
// The crash state is taken from the live database while the dial is still held,
// before the first run shuts down. Shutdown would abort the dial in-process and
// record dial_unconfirmed there, which is not the restart path under test.
func TestRestartAfterHangupDuringDialReleasesSlotWithoutRedial(t *testing.T) {
	const ctl = "v3:ctl-ending-crash"
	policy := func(c *config.Config) {
		c.CallPolicy = config.DefaultCallPolicy()
		c.CallPolicy.UserMaxConcurrentCalls = 1
		c.CallPolicy.UserMaxCallsPerHour = 1000
	}

	tel := newFakeTelnyx(t, ctl)
	first := startCallAppWith(t, tel, nil, policy)
	h := first.admin.headers(true)
	tpID := createTelephonyFixture(t, first, h)
	createAIFixture(t, first, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())

	release := tel.holdDials()
	defer release()
	status, _, body := sendJSON(t, first.client, http.MethodPost, first.base+"/api/v1/calls",
		policyCallBody(tpID, "+14155550111"), withIdempotency(h, "ending-crash-first"))
	if status != http.StatusAccepted {
		t.Fatalf("first call = %d %s", status, body)
	}
	callID, _ := decodeObject(t, body)["call_id"].(string)
	tel.waitDials(t, 1)

	status, _, body = send(t, first.client, http.MethodPost, hangupURL(first, callID), nil, h)
	if status != http.StatusOK {
		t.Fatalf("in-flight hangup = %d %s", status, body)
	}
	if got := waitStatusNow(t, first, h, callID); got != "ending" {
		t.Fatalf("call status before crash = %q, want ending", got)
	}

	// The crash point: the dial is still held, so no control id is recorded.
	// Copy the database now, while it is in that state.
	crashed := filepath.Join(t.TempDir(), "crashed")
	snapshotDataDir(t, first.dataDir, crashed)

	release()
	first.stop()

	// Restart on the crash snapshot with its own fake provider, so only the
	// restarted process's dials and hangups are counted.
	tel2 := newFakeTelnyx(t, ctl)
	second := restartCallAppWith(t, tel2, crashed, first.pubKey, first.privKey, policy)
	h2 := second.admin.headers(true)

	waitForCallReason(t, second, h2, callID, "dial_unconfirmed")
	if got := waitStatusNow(t, second, h2, callID); got != "failed" {
		t.Fatalf("crashed ending call status = %q, want failed", got)
	}

	// The slot is released, so a call that was blocked before is accepted.
	status, _, body = sendJSON(t, second.client, http.MethodPost, second.base+"/api/v1/calls",
		policyCallBody(tpID, "+14155550111"), withIdempotency(h2, "ending-crash-after"))
	if status != http.StatusAccepted {
		t.Fatalf("call after restart = %d %s, want 202 once the slot is released", status, body)
	}

	// Zero redials of the crashed call: the restarted process dials only the new
	// call. Nothing is hung up on the crashed call either, since it has no
	// control id.
	tel2.waitDials(t, 1)
	if n := len(tel2.dialBodies()); n != 1 {
		t.Fatalf("dials after restart = %d, want exactly one (the new call)", n)
	}
	if n := len(tel2.hangupsSeen()); n != 0 {
		t.Fatalf("hangups after restart = %d, want none for the crashed call", n)
	}
}

// snapshotDataDir copies the SQLite database from src into dst with VACUUM INTO,
// so a test can restart on a consistent copy of a live database.
func snapshotDataDir(t *testing.T, src, dst string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(src, databaseFileName)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`VACUUM INTO ?`, filepath.Join(dst, databaseFileName)); err != nil {
		t.Fatalf("snapshot database: %v", err)
	}
}
