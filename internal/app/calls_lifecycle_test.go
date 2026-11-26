package app

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/observability"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
)

// TestCallsEnabledWithKeyServesAndShutsDown boots the real app with a public
// URL and a master key, so the call operator and the dial worker are wired.
// Shutdown must still finish within the configured timeout.
func TestCallsEnabledWithKeyServesAndShutsDown(t *testing.T) {
	t.Setenv(secrets.EnvKey, validKeyB64())
	cfg := testConfig(t)
	cfg.PublicURL = "https://callharbor.example"
	logger := observability.NewLogger(io.Discard, slog.LevelError)
	ln := listen(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, logger, ln, runOptions{}) }()

	base := "http://" + ln.Addr().String()
	waitForStatus(t, http.DefaultClient, base+"/readyz", http.StatusOK)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run = %v", err)
		}
	case <-time.After(cfg.ShutdownTimeout + 5*time.Second):
		t.Fatal("run did not return after cancel")
	}
}

// TestCallsWithoutKeyStartAppAndFailClosed: OAuth and MCP do not need the
// master key, so a public URL without a key must still start the app. Calls
// stay off: make_call and POST /api/v1/calls refuse with calls_unavailable and
// write no queued row.
func TestCallsWithoutKeyStartAppAndFailClosed(t *testing.T) {
	t.Setenv(secrets.EnvKey, "")
	t.Setenv(secrets.EnvKeyFile, "")
	cfg := testConfig(t)
	cfg.PublicURL = "https://callharbor.example"
	assertCallsRefused(t, cfg)
}

// TestCallsWithoutPublicURLRefuse: without CALLHARBOR_PUBLIC_URL no call can be
// dialed, so calls are refused the same way, even with a master key present.
func TestCallsWithoutPublicURLRefuse(t *testing.T) {
	t.Setenv(secrets.EnvKey, validKeyB64())
	cfg := testConfig(t)
	cfg.PublicURL = ""
	assertCallsRefused(t, cfg)
}

// assertCallsRefused boots the real app with calls off, creates an owner, and
// sends one REST create. It must return calls_unavailable with no call row.
func assertCallsRefused(t *testing.T, cfg config.Config) {
	t.Helper()
	logger := observability.NewLogger(io.Discard, slog.LevelError)
	ln := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, logger, ln, runOptions{}) }()
	base := "http://" + ln.Addr().String()
	waitForStatus(t, http.DefaultClient, base+"/readyz", http.StatusOK)

	c := newE2EClient()
	admin := setupAdmin(t, c, base)
	h := admin.headers(true)
	h.Set("Idempotency-Key", "refused-1")
	status, _, body := sendJSON(t, c, http.MethodPost, base+"/api/v1/calls", map[string]any{
		"profile_id": "tp_none", "to": "+14155550111", "objective": "test",
	}, h)
	if status != http.StatusServiceUnavailable || !strings.Contains(string(body), "calls_unavailable") {
		t.Fatalf("create = %d %s, want 503 calls_unavailable", status, body)
	}
	if n := countCallRows(t, cfg.DataDir); n != 0 {
		t.Fatalf("refused create wrote %d call rows", n)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run = %v", err)
	}
}

// countCallRows counts calls rows in the data directory's database.
func countCallRows(t *testing.T, dataDir string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, databaseFileName)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM calls").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
