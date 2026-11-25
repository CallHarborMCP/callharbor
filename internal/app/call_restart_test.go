package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

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

	second := restartCallApp(t, tel, first.dataDir)
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
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, row := range []struct{ id, status, key string }{
		{"call_unconfirmed", "initiating", "restart-unconfirmed"},
		{"call_queued", "queued", "restart-queued"},
	} {
		if _, err := db.Exec(`INSERT INTO calls (id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at) VALUES (?, ?, ?, ?, ?, '+14155550111', ?, 300, ?, ?)`,
			row.id, owner, tpID, ai, row.key, row.status, now, now); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}
}

// restartCallApp runs the app again on an existing data dir. It skips setup,
// since the admin already exists, and logs in with the same password.
func restartCallApp(t *testing.T, tel *fakeTelnyx, dataDir string) *callTestEnv {
	t.Helper()
	t.Setenv(secrets.EnvKey, validKeyB64())
	ln, roots := newTLSListener(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.DataDir = dataDir
	cfg.PublicURL = "https://" + ln.Addr().String()
	opts := runOptions{
		TelnyxBaseURL:  tel.baseURL(),
		EndpointPolicy: realtime.EndpointPolicy{AllowPrivate: true},
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
