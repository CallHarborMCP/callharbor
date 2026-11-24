package main

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/config"
)

func TestRunRejectsInvalidConfig(t *testing.T) {
	t.Setenv(config.EnvDataDir, "relative/path")
	if err := run(); err == nil {
		t.Fatal("run accepted a relative data directory")
	}
}

func TestRunReportsListenFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	t.Setenv(config.EnvListenAddr, ln.Addr().String())
	t.Setenv(config.EnvDataDir, filepath.Join(t.TempDir()))
	if err := run(); err == nil {
		t.Fatal("run succeeded on an address already in use")
	}
}
