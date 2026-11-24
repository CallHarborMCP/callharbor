package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/aiprofile"
	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/oauth"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/server"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/web"
)

const databaseFileName = "callharbor.db"

const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
)

// openSecrets builds the secret service from the master key. It fails closed:
// when no key is configured and any secret rows exist, startup refuses, so
// stored credentials are never silently left unreadable or written in clear.
// With no key and no rows, the API is available but writes return an error.
func openSecrets(ctx context.Context, store *storage.Store, getenv func(string) string) (*secrets.Service, error) {
	key, err := secrets.LoadMasterKey(getenv)
	if errors.Is(err, secrets.ErrNoMasterKey) {
		existing, cerr := store.CountSecrets(ctx)
		if cerr != nil {
			return nil, fmt.Errorf("count secrets: %w", cerr)
		}
		if existing > 0 {
			return nil, fmt.Errorf("%d stored secrets exist but no master key is configured: set %s or %s", existing, secrets.EnvKeyFile, secrets.EnvKey)
		}
		return secrets.NewUnkeyedService(store, time.Now, newSecretID), nil
	}
	if err != nil {
		return nil, err
	}
	ring, err := secrets.NewKeyring(1, map[int][]byte{1: key})
	if err != nil {
		return nil, err
	}
	box, err := secrets.NewBox(ring)
	if err != nil {
		return nil, err
	}
	return secrets.NewService(box, store, time.Now, newSecretID)
}

// sessionUser adapts an identity session check to the oauth package, which
// needs only the user ID. The identity handler has already written the 401 or
// 403 response when it returns false.
func sessionUser(check func(http.ResponseWriter, *http.Request) (*identity.Principal, bool)) oauth.Authenticator {
	return func(w http.ResponseWriter, r *http.Request) (string, bool) {
		p, ok := check(w, r)
		if !ok {
			return "", false
		}
		return p.User.ID, true
	}
}

func newSecretID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "sec_" + base64.RawURLEncoding.EncodeToString(b[:])
}

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

	store, err := storage.Open(ctx, filepath.Join(cfg.DataDir, databaseFileName))
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	identitySvc, err := identity.NewService(store, time.Now)
	if err != nil {
		return fmt.Errorf("identity service: %w", err)
	}

	secretSvc, err := openSecrets(ctx, store, os.Getenv)
	if err != nil {
		return err
	}

	identityHandler := identity.NewHandler(identitySvc)
	profiles := aiprofile.NewHandler(identityHandler, store, time.Now)
	secretsAPI := secrets.NewHandler(identityHandler, secretSvc, store)
	routes := []func(*http.ServeMux){identityHandler.Register, profiles.Register, secretsAPI.Register}
	if cfg.PublicURL != "" {
		authz := oauth.NewServer(oauth.Config{PublicURL: cfg.PublicURL}, store,
			oauth.NewClientResolver(store, time.Now), time.Now)
		routes = append(routes, authz.Register, func(mux *http.ServeMux) {
			authz.RegisterConsentAPI(mux, sessionUser(identityHandler.Authenticate), sessionUser(identityHandler.AuthenticateMutating))
		})
	} else {
		logger.Info("OAuth and MCP are disabled until CALLHARBOR_PUBLIC_URL is set")
	}
	srv := server.New(logger, assets, routes...)
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
