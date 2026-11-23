package config

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
	if cfg.DataDir != "/data" {
		t.Errorf("DataDir = %q, want /data", cfg.DataDir)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 15s", cfg.ShutdownTimeout)
	}
}

func TestLoadValidOverrides(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(env(map[string]string{
		EnvListenAddr:      " 127.0.0.1:9000 ",
		EnvDataDir:         dir + string(filepath.Separator) + ".",
		EnvLogLevel:        "debug",
		EnvShutdownTimeout: "2s",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:9000" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if cfg.DataDir != filepath.Clean(dir) {
		t.Errorf("DataDir = %q, want cleaned %q", cfg.DataDir, filepath.Clean(dir))
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 2*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 2s", cfg.ShutdownTimeout)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		values  map[string]string
		wantKey string
	}{
		{"address without port", map[string]string{EnvListenAddr: "localhost"}, EnvListenAddr},
		{"port zero", map[string]string{EnvListenAddr: ":0"}, EnvListenAddr},
		{"port too large", map[string]string{EnvListenAddr: ":70000"}, EnvListenAddr},
		{"non-numeric port", map[string]string{EnvListenAddr: ":http"}, EnvListenAddr},
		{"relative data dir", map[string]string{EnvDataDir: "data"}, EnvDataDir},
		{"unknown log level", map[string]string{EnvLogLevel: "verbose"}, EnvLogLevel},
		{"unparseable timeout", map[string]string{EnvShutdownTimeout: "soon"}, EnvShutdownTimeout},
		{"zero timeout", map[string]string{EnvShutdownTimeout: "0s"}, EnvShutdownTimeout},
		{"negative timeout", map[string]string{EnvShutdownTimeout: "-1s"}, EnvShutdownTimeout},
		{"timeout above maximum", map[string]string{EnvShutdownTimeout: "10m"}, EnvShutdownTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.values))
			if err == nil {
				t.Fatalf("Load() error = nil, want error mentioning %s", tt.wantKey)
			}
			if !strings.Contains(err.Error(), tt.wantKey) {
				t.Errorf("error = %q, want it to mention %s", err, tt.wantKey)
			}
		})
	}
}

func TestLoadReportsAllErrorsTogether(t *testing.T) {
	_, err := Load(env(map[string]string{
		EnvListenAddr: "nope",
		EnvDataDir:    "relative",
		EnvLogLevel:   "loud",
	}))
	if err == nil {
		t.Fatal("Load() error = nil, want error")
	}
	for _, key := range []string{EnvListenAddr, EnvDataDir, EnvLogLevel} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error = %q, want it to mention %s", err, key)
		}
	}
}
