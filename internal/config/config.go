package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
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

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
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
