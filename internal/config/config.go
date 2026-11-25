package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	EnvListenAddr      = "CALLHARBOR_LISTEN_ADDR"
	EnvDataDir         = "CALLHARBOR_DATA_DIR"
	EnvLogLevel        = "CALLHARBOR_LOG_LEVEL"
	EnvShutdownTimeout = "CALLHARBOR_SHUTDOWN_TIMEOUT"
	// EnvPublicURL is the absolute public origin of this installation, for
	// example https://callharbor.example.com. It is the OAuth issuer and the
	// MCP resource base. When unset, OAuth and MCP routes are not mounted.
	EnvPublicURL = "CALLHARBOR_PUBLIC_URL"

	defaultListenAddr      = ":8080"
	defaultDataDir         = "/data"
	defaultShutdownTimeout = 15 * time.Second
	maxShutdownTimeout     = 5 * time.Minute
)

type Config struct {
	ListenAddr      string
	DataDir         string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	// PublicURL is the normalized origin (scheme://host[:port], no trailing
	// slash). Empty means OAuth/MCP routes are disabled.
	PublicURL string
}

func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		ListenAddr:      value(getenv, EnvListenAddr, defaultListenAddr),
		DataDir:         value(getenv, EnvDataDir, defaultDataDir),
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: defaultShutdownTimeout,
	}

	var errs []error

	if err := validateListenAddr(cfg.ListenAddr); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvListenAddr, err))
	}

	if !filepath.IsAbs(cfg.DataDir) {
		errs = append(errs, fmt.Errorf("%s: must be an absolute path", EnvDataDir))
	} else {
		cfg.DataDir = filepath.Clean(cfg.DataDir)
	}

	if raw := strings.TrimSpace(getenv(EnvLogLevel)); raw != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(raw)); err != nil {
			errs = append(errs, fmt.Errorf("%s: must be one of debug, info, warn or error", EnvLogLevel))
		}
	}

	if raw := strings.TrimSpace(getenv(EnvShutdownTimeout)); raw != "" {
		timeout, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: must be a duration such as 15s", EnvShutdownTimeout))
		case timeout <= 0 || timeout > maxShutdownTimeout:
			errs = append(errs, fmt.Errorf("%s: must be greater than 0 and at most %s", EnvShutdownTimeout, maxShutdownTimeout))
		default:
			cfg.ShutdownTimeout = timeout
		}
	}

	if raw := strings.TrimSpace(getenv(EnvPublicURL)); raw != "" {
		origin, err := parsePublicURL(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvPublicURL, err))
		} else {
			cfg.PublicURL = origin
		}
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// parsePublicURL accepts an absolute origin. https is required except for
// loopback hosts, which may use http for local development. Paths, queries,
// fragments, userinfo and empty hosts are rejected. A trailing slash is
// normalized away.
func parsePublicURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("must be an absolute origin such as https://callharbor.example.com")
	}
	if u.Opaque != "" || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", errors.New("must be an origin with no userinfo, query or fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("must not contain a path")
	}
	host := u.Hostname()
	if host == "" {
		return "", errors.New("must include a host")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(host) {
			return "", errors.New("http is allowed only for loopback hosts; use https")
		}
	default:
		return "", errors.New("scheme must be https (or http for loopback hosts)")
	}
	// The origin is normalized to one spelling: a lowercase host and no port
	// when it is the scheme's default. Metadata and issuer checks compare it.
	port := u.Port()
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", errors.New("port must be between 1 and 65535")
		}
	}
	origin := u.Scheme + "://" + strings.ToLower(host)
	if strings.Contains(host, ":") {
		origin = u.Scheme + "://[" + strings.ToLower(host) + "]" // IPv6 literal keeps its brackets
	}
	if port != "" && !isDefaultPort(u.Scheme, port) {
		origin += ":" + port
	}
	return origin, nil
}

// isDefaultPort reports whether port is the default for scheme, which the
// origin omits.
func isDefaultPort(scheme, port string) bool {
	return (scheme == "https" && port == "443") || (scheme == "http" && port == "80")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateListenAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}

func value(getenv func(string) string, key, fallback string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return fallback
}
