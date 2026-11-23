package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/server"
	"github.com/CallHarborMCP/callharbor/web"
)

const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
)

func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, ln net.Listener) error {
	info, err := os.Stat(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("data directory unavailable: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("data directory %s is not a directory", cfg.DataDir)
	}
	probe, err := os.CreateTemp(cfg.DataDir, ".write-probe-*")
	if err != nil {
		return fmt.Errorf("data directory %s is not writable: %w", cfg.DataDir, err)
	}
	_ = probe.Close()
	if err := os.Remove(probe.Name()); err != nil {
		return fmt.Errorf("data directory %s cleanup: %w", cfg.DataDir, err)
	}

	assets, err := web.Assets()
	if err != nil {
		return fmt.Errorf("embedded assets: %w", err)
	}

	srv := server.New(logger, assets)
	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	srv.SetReady(true)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.Serve(ln)
	}()
	logger.Info("server listening", "addr", ln.Addr().String(), "data_dir", cfg.DataDir)

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	srv.SetReady(false)
	logger.Info("shutdown started", "timeout", cfg.ShutdownTimeout.String())

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	logger.Info("shutdown complete")
	return nil
}
