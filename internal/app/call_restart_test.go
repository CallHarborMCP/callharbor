package app

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/observability"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	_ "modernc.org/sqlite"
)

// TestRestartFailsUnconfirmedAndDialsQueuedOnce covers restart recovery through
// the real app. The first run creates the profiles and stops. The test then
// seeds the state a crash leaves behind: one call claimed into initiating with
// no provider control id, and one still queued. A second run on the same data
// dir must fail the unconfirmed call with dial_unconfirmed, never redial it,
// and dial the queued call exactly once.
func TestRestartFailsUnconfirmedAndDialsQueuedOnce(t *testing.T) {
	tel := newFakeTelnyx(t, "v3:ctl-restart")
	first := startCallApp(t, tel, nil)
	h := first.admin.headers(true)
	tpID := createTelephonyFixture(t, first, h)
	createAIFixture(t, first, h, realtime.ProtocolRealtime, newFakeAI(t, realtime.ProtocolRealtime).endpoint())
	first.stop()
	if n := len(tel.dialBodies()); n != 0 {
		t.Fatalf("first run dialed %d times, want none before the restart", n)
	}

	seedRestartRows(t, first.dataDir, tpID)

	second := restartCallApp(t, tel, first.dataDir, first.pubKey, first.privKey)
	h2 := second.admin.headers(true)

	waitForCallReason(t, second, h2, "call_unconfirmed", "dial_unconfirmed")
	if got := waitStatusNow(t, second, h2, "call_unconfirmed"); got != "failed" {
		t.Fatalf("unconfirmed call status = %q, want failed", got)
	}

	// Exactly one dial: the queued call. The unconfirmed call is never redialed.
	tel.waitDials(t, 1)
	waitQuiet(tel)
	if n := len(tel.dialBodies()); n != 1 {
		t.Fatalf("dials after restart = %d, want exactly one", n)
	}
	if got := waitStatusNow(t, second, h2, "call_queued"); got == "queued" {
		t.Fatalf("queued call was not dialed after restart")
	}
}

// seedRestartRows inserts the two rows a crash leaves behind, owned by the
// admin and using the profiles created by the first run.
func seedRestartRows(t *testing.T, dataDir, tpID string) {
	t.Helper()
	seedRestartRowsFor(t, dataDir, tpID, []restartRow{
		{"call_unconfirmed", "initiating", "restart-unconfirmed"},
		{"call_queued", "queued", "restart-queued"},
	})
}

// restartRow is one call a test seeds into the crash state.
type restartRow struct{ id, status, key string }

// seedRestartRowsFor inserts the given calls, owned by the admin, with their
// brief rows, so each one is visible to webhook and media lookups.
func seedRestartRowsFor(t *testing.T, dataDir, tpID string, rows []restartRow) {
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
	// The store writes timestamps as fixed-width milliseconds. The seed uses the
	// same format, or the reconciliation sweep cannot parse created_at.
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	for _, row := range rows {
		if _, err := db.Exec(`INSERT INTO calls (id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at) VALUES (?, ?, ?, ?, ?, '+14155550111', ?, 300, ?, ?)`,
			row.id, owner, tpID, ai, row.key, row.status, now, now); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
		// Every call has a brief row; media and webhook lookups join it. A row
		// without one is invisible to them, as a real call never is.
		if _, err := db.Exec(`INSERT INTO call_briefs (call_id, owner_user_id, version, content_json, content_sha256, created_at) VALUES (?, ?, 1, '{"version":1,"objective":"Confirm"}', ?, ?)`,
			row.id, owner, strings.Repeat("0", 64), now); err != nil {
			t.Fatalf("seed brief %s: %v", row.id, err)
		}
	}
}

// restartCallApp runs the app again on an existing data dir. It skips setup,
// since the admin already exists, and logs in with the same password.
func restartCallApp(t *testing.T, tel *fakeTelnyx, dataDir string, pub ed25519.PublicKey, priv ed25519.PrivateKey) *callTestEnv {
	t.Helper()
	return restartCallAppWith(t, tel, dataDir, pub, priv, nil)
}

// restartCallAppWith is restartCallApp with a config hook, for a test that
// needs a policy on the restarted run.
func restartCallAppWith(t *testing.T, tel *fakeTelnyx, dataDir string, pub ed25519.PublicKey, priv ed25519.PrivateKey, adjust func(*config.Config)) *callTestEnv {
	t.Helper()
	return restartCallAppAt(t, tel, dataDir, pub, priv, adjust, nil)
}

// restartCallAppAt is restartCallAppWith with a clock. The app's worker,
// webhooks and hangup paths read now, so a test can move the reconciliation
// deadlines forward without a real wait. A nil now means the real clock.
func restartCallAppAt(t *testing.T, tel *fakeTelnyx, dataDir string, pub ed25519.PublicKey, priv ed25519.PrivateKey, adjust func(*config.Config), now func() time.Time) *callTestEnv {
	t.Helper()
	t.Setenv(secrets.EnvKey, validKeyB64())
	ln, roots := newTLSListener(t)
	cfg := testConfig(t)
	cfg.DataDir = dataDir
	cfg.PublicURL = "https://" + ln.Addr().String()
	if adjust != nil {
		adjust(&cfg)
	}
	opts := runOptions{
		TelnyxBaseURL:  tel.baseURL(),
		EndpointPolicy: realtime.EndpointPolicy{AllowPrivate: true},
		Now:            now,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, observability.NewLogger(io.Discard, slog.LevelError), ln, opts)
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("run = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("run did not return after cancel")
			}
		})
	}
	t.Cleanup(stop)
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true,
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	waitForStatus(t, client, cfg.PublicURL+"/readyz", http.StatusOK)
	return &callTestEnv{t: t, base: cfg.PublicURL, client: client, pubKey: pub, privKey: priv,
		roots: roots, tel: tel, dataDir: dataDir, stop: stop, admin: login(t, client, cfg.PublicURL, "admin")}
}
