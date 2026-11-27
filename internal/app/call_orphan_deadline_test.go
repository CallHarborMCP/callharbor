package app

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// TestOldTerminalOrphanGetsNoHangupAfterDeadline covers review should-fix 3. Two
// terminal calls have a provider control id and no hangup marker, the state a
// call from before this change leaves behind. One was created two hours ago, so
// its leg deadline has passed and Telnyx has ended it. The other was created
// just now and is still inside its window.
//
// The restarted worker's sweep runs on a fake clock, past the retry interval, so
// it really runs and considers both rows. It must hang up only the call inside
// its window. Hanging up the old one is the regression this guards against.
func TestOldTerminalOrphanGetsNoHangupAfterDeadline(t *testing.T) {
	const ctlOld = "v3:ctl-old-orphan"
	const ctlFresh = "v3:ctl-fresh-orphan"
	policy := func(c *config.Config) {
		c.CallPolicy = config.DefaultCallPolicy()
		c.CallPolicy.UserMaxConcurrentCalls = 3
		c.CallPolicy.UserMaxCallsPerHour = 1000
	}
	tel := newFakeTelnyx(t, ctlOld)
	first := startCallAppWith(t, tel, nil, policy)
	h := first.admin.headers(true)
	tpID := createTelephonyFixture(t, first, h)
	createAIFixture(t, first, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	first.stop()

	t0 := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	seedTerminalOrphans(t, first.dataDir, tpID, t0, map[string]string{
		"call_old_terminal":   ctlOld,
		"call_fresh_terminal": ctlFresh,
	})

	// Created two hours before the clock's start: far past the 600 s default cap
	// plus ring timeout and grace. The fresh call is created 100 s before t0, which
	// is inside the window (cap 600 s + ring + grace).
	setCreatedAt(t, first.dataDir, "call_old_terminal", t0.Add(-2*time.Hour))
	setCreatedAt(t, first.dataDir, "call_fresh_terminal", t0.Add(-100*time.Second))

	clk := &testClock{now: t0}
	_ = restartCallAppAt(t, tel, first.dataDir, first.pubKey, first.privKey, policy, clk.Now)

	// Move past the retry interval so the sweep considers the orphans. The fresh
	// call is still inside its window, so it must be hung up. The old one must not.
	clk.Set(t0.Add(61 * time.Second))
	waitHangups(t, tel, 1)
	waitQuiet(tel)

	got := tel.hangupsSeen()
	for _, h := range got {
		if h[:len(ctlOld)] == ctlOld {
			t.Fatalf("old terminal orphan was hung up after its deadline: %v", got)
		}
	}
	if len(got) != 1 || got[0][:len(ctlFresh)] != ctlFresh {
		t.Fatalf("hangups = %v, want exactly one for the fresh orphan", got)
	}
}

// seedTerminalOrphans inserts terminal calls with a provider control id and no
// hangup marker, the state a call from before the hangup markers existed leaves.
// Terminal rows need ended_at, which the shared restart seed does not set, so
// these rows are written here. Each call is owned by the admin, with a brief.
// Timestamps are set to at, the fake clock's start, so the sweep compares them
// against the same clock the test moves, not the wall clock. setCreatedAt then
// places each call's created_at; ended_at = at stays at or after it.
func seedTerminalOrphans(t *testing.T, dataDir, tpID string, at time.Time, controls map[string]string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, databaseFileName)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var owner, ai string
	if err := db.QueryRow(`SELECT owner_user_id FROM telephony_profiles WHERE id = ?`, tpID).Scan(&owner); err != nil {
		t.Fatalf("owner of profile: %v", err)
	}
	if err := db.QueryRow(`SELECT id FROM ai_profiles WHERE owner_user_id = ? LIMIT 1`, owner).Scan(&ai); err != nil {
		t.Fatalf("ai profile: %v", err)
	}
	now := at.UTC().Format("2006-01-02T15:04:05.000Z")
	for id, ctl := range controls {
		if _, err := db.Exec(`INSERT INTO calls (id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, outcome_reason_code, provider_call_control_id, ended_at, max_duration_seconds, created_at, updated_at) VALUES (?, ?, ?, ?, ?, '+14155550111', 'failed', 'hangup', ?, ?, 600, ?, ?)`,
			id, owner, tpID, ai, "restart-"+id, ctl, now, now, now); err != nil {
			t.Fatalf("seed terminal %s: %v", id, err)
		}
		if _, err := db.Exec(`INSERT INTO call_briefs (call_id, owner_user_id, version, content_json, content_sha256, created_at) VALUES (?, ?, 1, '{"version":1,"objective":"Confirm"}', ?, ?)`,
			id, owner, "0000000000000000000000000000000000000000000000000000000000000000", now); err != nil {
			t.Fatalf("seed brief %s: %v", id, err)
		}
	}
}

// setCreatedAt sets a seeded call's created_at, so a test can place it inside or
// past its leg deadline.
func setCreatedAt(t *testing.T, dataDir, callID string, at time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, databaseFileName)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE calls SET created_at = ? WHERE id = ?`, at.UTC().Format("2006-01-02T15:04:05.000Z"), callID); err != nil {
		t.Fatalf("set created_at for %s: %v", callID, err)
	}
}
