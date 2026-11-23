package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/observability"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		ListenAddr:      "127.0.0.1:0",
		DataDir:         t.TempDir(),
		LogLevel:        slog.LevelError,
		ShutdownTimeout: 5 * time.Second,
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func TestRunServesThenShutsDownCleanly(t *testing.T) {
	cfg := testConfig(t)
	logger := observability.NewLogger(io.Discard, slog.LevelError)
	ln := listen(t)
	base := fmt.Sprintf("http://%s", ln.Addr().String())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, logger, ln) }()

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	waitForStatus(t, client, base+"/readyz", http.StatusOK)

	resp, err := client.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d", resp.StatusCode)
	}
	client.CloseIdleConnections()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil after graceful shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not return after context cancellation")
	}

	if _, err := http.Get(base + "/healthz"); err == nil {
		t.Fatal("server still accepting connections after shutdown")
	}
}

func TestRunRejectsMissingDataDirectory(t *testing.T) {
	cfg := testConfig(t)
	cfg.DataDir = filepath.Join(t.TempDir(), "missing")
	logger := observability.NewLogger(io.Discard, slog.LevelError)

	err := Run(context.Background(), cfg, logger, listen(t))
	if err == nil {
		t.Fatal("Run() error = nil, want data directory error")
	}
}

func TestRunRejectsDataPathThatIsAFile(t *testing.T) {
	cfg := testConfig(t)
	file := filepath.Join(cfg.DataDir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = file
	logger := observability.NewLogger(io.Discard, slog.LevelError)

	if err := Run(context.Background(), cfg, logger, listen(t)); err == nil {
		t.Fatal("Run() error = nil, want not-a-directory error")
	}
}

func waitForStatus(t *testing.T, client *http.Client, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not return %d before deadline (last err: %v)", url, want, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
