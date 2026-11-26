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

func TestLoadPublicURL(t *testing.T) {
	valid := map[string]string{
		"https://callharbor.example.com":  "https://callharbor.example.com",
		"https://callharbor.example.com/": "https://callharbor.example.com",
		"https://Example.com:8443/":       "https://example.com:8443",
		"http://127.0.0.1:8080":           "http://127.0.0.1:8080",
		"http://localhost:8080/":          "http://localhost:8080",
		"http://[::1]:8080":               "http://[::1]:8080",
	}
	for in, want := range valid {
		cfg, err := Load(env(map[string]string{EnvPublicURL: in}))
		if err != nil {
			t.Errorf("Load(%q) error = %v", in, err)
			continue
		}
		if cfg.PublicURL != want {
			t.Errorf("Load(%q).PublicURL = %q, want %q", in, cfg.PublicURL, want)
		}
	}

	unset, err := Load(env(nil))
	if err != nil || unset.PublicURL != "" {
		t.Fatalf("unset PublicURL = %q, err = %v; want empty", unset.PublicURL, err)
	}

	invalid := []string{
		"http://callharbor.example.com",
		"https://callharbor.example.com/mcp",
		"https://callharbor.example.com?x=1",
		"https://callharbor.example.com#frag",
		"https://user:pw@callharbor.example.com",
		"ftp://callharbor.example.com",
		"callharbor.example.com",
		"https://",
		"https://callharbor.example.com:99999",
	}
	for _, in := range invalid {
		if _, err := Load(env(map[string]string{EnvPublicURL: in})); err == nil {
			t.Errorf("Load(%q) error = nil, want rejection", in)
		} else if !strings.Contains(err.Error(), EnvPublicURL) {
			t.Errorf("Load(%q) error %q does not name %s", in, err, EnvPublicURL)
		}
	}
}

// TestLoadPublicURLNormalizesHostAndDefaultPort: the host is lowercased and a
// scheme's default port is dropped, so one origin has one spelling. The
// origin is compared and embedded in metadata, so these must match.
func TestLoadPublicURLNormalizesHostAndDefaultPort(t *testing.T) {
	cases := map[string]string{
		"https://Callharbor.Example.COM:443":  "https://callharbor.example.com",
		"https://callharbor.example.com:443/": "https://callharbor.example.com",
		"http://LOCALHOST:80":                 "http://localhost",
		"https://Example.com:8443":            "https://example.com:8443",
	}
	for in, want := range cases {
		cfg, err := Load(env(map[string]string{EnvPublicURL: in}))
		if err != nil {
			t.Errorf("Load(%q) error = %v", in, err)
			continue
		}
		if cfg.PublicURL != want {
			t.Errorf("Load(%q).PublicURL = %q, want %q", in, cfg.PublicURL, want)
		}
	}
}

func TestLoadTrustedProxiesDefaultsToNone(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Fatalf("TrustedProxies = %v, want none", cfg.TrustedProxies)
	}
}

func TestLoadTrustedProxiesParsesPrefixesAndAddresses(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		EnvTrustedProxies: " 10.0.0.0/8 , 192.0.2.7,2001:db8::/32,  ::1 ",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"10.0.0.0/8", "192.0.2.7/32", "2001:db8::/32", "::1/128"}
	if len(cfg.TrustedProxies) != len(want) {
		t.Fatalf("TrustedProxies = %v, want %v", cfg.TrustedProxies, want)
	}
	for i, p := range cfg.TrustedProxies {
		if p.String() != want[i] {
			t.Errorf("TrustedProxies[%d] = %s, want %s", i, p, want[i])
		}
	}
}

// TestLoadTrustedProxiesUnmapsIPv4MappedEntries: peers are compared in their
// IPv4 form, so a mapped entry must be stored in that form too.
func TestLoadTrustedProxiesUnmapsIPv4MappedEntries(t *testing.T) {
	cfg, err := Load(env(map[string]string{EnvTrustedProxies: "::ffff:10.1.2.3, ::ffff:172.16.0.0/108"}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.TrustedProxies[0].String() != "10.1.2.3/32" {
		t.Errorf("single mapped address = %s, want 10.1.2.3/32", cfg.TrustedProxies[0])
	}
	if cfg.TrustedProxies[1].String() != "172.16.0.0/12" {
		t.Errorf("mapped prefix = %s, want 172.16.0.0/12", cfg.TrustedProxies[1])
	}
}

func TestLoadRejectsInvalidTrustedProxies(t *testing.T) {
	invalid := []string{
		"10.0.0.0/33",
		"10.0.0.1/",
		"not-an-ip",
		"10.0.0.256",
		"fe80::1%eth0",
		"fe80::/64%eth0",
		"10.0.0.0/8,",
		",10.0.0.0/8",
		"10.0.0.0/8,,192.0.2.1",
		"10.0.0.0/8 192.0.2.1",
	}
	for _, in := range invalid {
		_, err := Load(env(map[string]string{EnvTrustedProxies: in}))
		if err == nil {
			t.Errorf("Load(%q) error = nil, want rejection", in)
			continue
		}
		if !strings.Contains(err.Error(), EnvTrustedProxies) {
			t.Errorf("Load(%q) error %q does not name %s", in, err, EnvTrustedProxies)
		}
	}
}

func TestLoadMaxCallBriefBytes(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxCallBriefBytes != DefaultMaxCallBriefBytes {
		t.Fatalf("default = %d, want %d", cfg.MaxCallBriefBytes, DefaultMaxCallBriefBytes)
	}
	cfg, err = Load(env(map[string]string{EnvMaxCallBriefBytes: " 65536 "}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxCallBriefBytes != 65536 {
		t.Fatalf("override = %d, want 65536", cfg.MaxCallBriefBytes)
	}
	for _, bad := range []string{"12x", "1023", "262145", "-5"} {
		if _, err := Load(env(map[string]string{EnvMaxCallBriefBytes: bad})); err == nil {
			t.Fatalf("%s=%q accepted", EnvMaxCallBriefBytes, bad)
		} else if !strings.Contains(err.Error(), EnvMaxCallBriefBytes) {
			t.Fatalf("error does not name the variable: %v", err)
		}
	}
}
