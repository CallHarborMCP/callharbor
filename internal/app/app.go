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
	"runtime/debug"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/aiprofile"
	"github.com/CallHarborMCP/callharbor/internal/api"
	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/callstream"
	"github.com/CallHarborMCP/callharbor/internal/callworker"
	"github.com/CallHarborMCP/callharbor/internal/config"
	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/mcp"
	"github.com/CallHarborMCP/callharbor/internal/oauth"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/realtime/livews"
	"github.com/CallHarborMCP/callharbor/internal/realtime/realtimews"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/server"
	"github.com/CallHarborMCP/callharbor/internal/service"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/telephonyprofile"
	"github.com/CallHarborMCP/callharbor/internal/voicecall"
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

// buildVersion reports the module version from build info, or "dev" when the
// binary was built without version metadata.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "dev"
	}
	return info.Main.Version
}

func newSecretID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "sec_" + base64.RawURLEncoding.EncodeToString(b[:])
}

func newTelephonyProfileID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "tp_" + base64.RawURLEncoding.EncodeToString(b[:])
}

// callStreamTokens derives the per-call stream tokens from the master key. Calls
// need it, so without a key calls stay off and the caller sees why.
func callStreamTokens(getenv func(string) string) (*callstream.Tokens, error) {
	key, err := secrets.LoadMasterKey(getenv)
	if err != nil {
		return nil, fmt.Errorf("calls need the secrets master key: %w", err)
	}
	tokens, err := callstream.NewTokens(key)
	if err != nil {
		return nil, fmt.Errorf("call stream tokens: %w", err)
	}
	return tokens, nil
}

// mediaFormat is the provider wire format for every AI session: G.711 mu-law at
// 8 kHz, which both supported protocols carry.
var mediaFormat = audio.Format{Encoding: audio.PCMU, SampleRate: 8000}

// errNoWebhookKey reports a call whose telephony profile has no webhook key.
// Its webhooks and media are refused, so no call is served without a key.
var errNoWebhookKey = errors.New("calls: telephony profile has no webhook public key")

// newCallIngress builds the per-call webhook and media routes. Each webhook is
// verified with the owning telephony profile's key, read per request. Each media
// stream is authenticated by its path token and the Telnyx handshake.
func newCallIngress(store *storage.Store, sec *secrets.Service, tokens *callstream.Tokens, legs callworker.LegResolver, cfg config.Config, opts runOptions, logger *slog.Logger) ([]func(*http.ServeMux), error) {
	// The public origin must map to a WebSocket origin. Checking it here means
	// StreamURL cannot fail later for the configured origin.
	if _, err := callworker.StreamURL(cfg.PublicURL, "probe", "probe"); err != nil {
		return nil, fmt.Errorf("call stream url: %w", err)
	}
	webhooks, err := callstream.NewWebhooks(store, opts.Now)
	if err != nil {
		return nil, fmt.Errorf("call webhooks: %w", err)
	}
	keys := func(ctx context.Context, ownerUserID, secretID string) (string, error) {
		if secretID == "" {
			return "", errNoWebhookKey
		}
		b, err := sec.Resolve(ctx, ownerUserID, secretID, secrets.PurposeTelnyxWebhookKey)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	lookup := func(ctx context.Context, callID string) (string, error) {
		c, err := store.LoadCallForMedia(ctx, callID)
		if err != nil {
			return "", err
		}
		return keys(ctx, c.TelephonyOwnerUserID, c.WebhookKeySecretID)
	}
	resolver, err := voicecall.NewResolver(store, sec, mediaFormat, opts.EndpointPolicy)
	if err != nil {
		return nil, fmt.Errorf("call ai resolver: %w", err)
	}
	dialers := opts.AIDialers
	if dialers == nil {
		dialers = realtime.Dialers{
			realtime.ProtocolRealtime: realtimews.Dialer,
			realtime.ProtocolLive:     livews.Dialer,
		}
	}
	media := &callstream.Media{
		Store:    store,
		Tokens:   tokens,
		Resolver: resolver,
		Dialers:  dialers,
		Format:   mediaFormat,
		MaxBytes: cfg.MaxCallBriefBytes,
		Now:      opts.Now,
		Logger:   logger,
		Keys:     keys,
		Verifier: webhooks.Verifier,
		// The URL was validated above, so the only error here is one the
		// startup check already ruled out.
		StreamURL: func(callID, token string) string {
			u, _ := callworker.StreamURL(cfg.PublicURL, callID, token)
			return u
		},
		// The same resolver and command id as shutdown, so a retry is one leg.
		Hangup: func(ctx context.Context, leg storage.LiveCallLeg) error {
			return callworker.HangupLeg(ctx, legs, leg)
		},
	}
	return []func(*http.ServeMux){func(mux *http.ServeMux) {
		mux.Handle("POST /telnyx/calls/{call_id}/webhook", webhooks.Handler(lookup))
		mux.Handle("GET /telnyx/calls/{call_id}/media/{token}", media.Handler())
	}}, nil
}

// newCallWorker builds the dial worker. It is enabled whenever calls are
// (PublicURL set and the master key present), so queued calls are dialed. The
// per-call ingress is mounted in the same branch.
func newCallWorker(store *storage.Store, resolver callworker.ProfileResolver, cfg config.Config, logger *slog.Logger, opts runOptions, tokens *callstream.Tokens) (*callworker.Worker, callworker.ProductionPlanner, error) {
	planner := callworker.ProductionPlanner{
		Profiles: resolver,
		Dialers:  callworker.TelnyxDialerFactory(opts.TelnyxBaseURL),
		Routes: callworker.PublicRoutes{
			PublicURL:   cfg.PublicURL,
			StreamToken: tokens.Token,
		},
		MaxCallSeconds: maxCallSeconds,
		Codec:          mediaFormat,
	}
	w, err := callworker.New(store, planner, callworker.Options{
		Enabled: true,
		Now:     opts.Now,
		Logger:  logger,
	})
	if err != nil {
		return nil, callworker.ProductionPlanner{}, fmt.Errorf("call worker: %w", err)
	}
	return w, planner, nil
}

// maxCallSeconds bounds a dial's time_limit_secs. Each call's own
// max_duration_seconds is applied per call; this is the outer cap.
const maxCallSeconds = 7200

// telnyxProvider builds the read-only Telnyx client for one plaintext key at
// baseURL. Production passes telnyx.DefaultBaseURL. Validation issues GET
// requests only.
func telnyxProvider(baseURL string) telephonyprofile.ProviderFactory {
	return func(apiKey string) (telephonyprofile.Provider, error) {
		c, err := telnyx.NewClient(baseURL, apiKey, nil)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// runOptions are test seams. Production Run passes the zero value, which keeps
// the production Telnyx root, the default endpoint policy and the real clock.
// Tests use them to reach a loopback fake Telnyx and loopback AI endpoints.
type runOptions struct {
	// TelnyxBaseURL overrides telnyx.DefaultBaseURL for dials.
	TelnyxBaseURL string
	// EndpointPolicy overrides the default SSRF policy for AI endpoints.
	EndpointPolicy realtime.EndpointPolicy
	// AIDialers replaces the AI protocol adapters. Tests use it to refuse a
	// session with realtime.ErrCapability, which the shipped matrices cannot
	// produce for the fixed media format. nil keeps the real adapters.
	AIDialers realtime.Dialers
	// Now overrides the clock. nil means time.Now.
	Now func() time.Time
}

// Run serves the application until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, ln net.Listener) error {
	return run(ctx, cfg, logger, ln, runOptions{})
}

func run(ctx context.Context, cfg config.Config, logger *slog.Logger, ln net.Listener, opts runOptions) error {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.TelnyxBaseURL == "" {
		opts.TelnyxBaseURL = telnyx.DefaultBaseURL
	}
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
	telSvc, err := telephonyprofile.New(store, secretSvc, telnyxProvider(opts.TelnyxBaseURL), time.Now, newTelephonyProfileID, logger)
	if err != nil {
		return fmt.Errorf("telephony profile service: %w", err)
	}
	// Calls need a public origin for the provider's webhook and media URLs.
	// Without one, MakeCall fails closed instead of queuing a call that can
	// never be dialed.
	svc := service.New(store)
	var worker *callworker.Worker
	var legResolver callworker.LegResolver
	var callRoutes []func(*http.ServeMux)
	tokens, tokenErr := callStreamTokens(os.Getenv)
	if cfg.PublicURL != "" && tokenErr != nil {
		// Calls need the master key for stream tokens. OAuth and MCP do not,
		// so the rest of the app still starts; MakeCall fails closed.
		logger.Warn("calls are disabled: they need the secrets master key", "reason", tokenErr.Error())
	}
	if cfg.PublicURL != "" && tokenErr == nil {
		var planner callworker.ProductionPlanner
		worker, planner, err = newCallWorker(store, telSvc, cfg, logger, opts, tokens)
		if err != nil {
			return err
		}
		legResolver = planner
		callOp, err := service.NewCallOperator(store, service.CallDeps{
			Resolver:      telSvc,
			MaxBriefBytes: cfg.MaxCallBriefBytes,
			Now:           time.Now,
			NewCallID:     service.NewCallID,
			Notify:        worker.Wake,
		})
		if err != nil {
			return fmt.Errorf("call service: %w", err)
		}
		svc.WithCalls(callOp)
		callRoutes, err = newCallIngress(store, secretSvc, tokens, legResolver, cfg, opts, logger)
		if err != nil {
			return err
		}
	} else if cfg.PublicURL == "" {
		logger.Info("calls are disabled until CALLHARBOR_PUBLIC_URL is set")
	}
	restV1 := api.NewHandler(identityHandler, svc, telSvc, logger)
	routes := []func(*http.ServeMux){identityHandler.Register, profiles.Register, secretsAPI.Register, restV1.Register}
	routes = append(routes, callRoutes...)
	if cfg.PublicURL != "" {
		authz := oauth.NewServer(oauth.Config{PublicURL: cfg.PublicURL, TrustedProxies: cfg.TrustedProxies}, store,
			oauth.NewClientResolver(store, time.Now), time.Now, logger)
		mcpHandler := mcp.New(svc, buildVersion(), logger)
		routes = append(routes, authz.Register, func(mux *http.ServeMux) {
			authz.RegisterConsentAPI(mux, sessionUser(identityHandler.Authenticate), sessionUser(identityHandler.AuthenticateMutating))
			authz.RegisterMCP(mux, mcpHandler.Wrap)
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

	// The worker is detached from ctx cancellation: ctx ending starts shutdown,
	// but an in-flight dial must still finish (or be aborted and recorded as
	// dial_unconfirmed) within the shutdown deadline. Stop and Abort end it.
	workerDone := make(chan error, 1)
	if worker != nil {
		workerCtx := context.WithoutCancel(ctx)
		go func() {
			workerDone <- worker.Run(workerCtx)
		}()
	} else {
		close(workerDone)
	}

	srv.SetReady(true)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.Serve(ln)
	}()
	logger.Info("server listening", "addr", ln.Addr().String(), "data_dir", cfg.DataDir)

	select {
	case err := <-serveErr:
		if worker != nil {
			worker.Stop()
			worker.Abort()
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	srv.SetReady(false)
	logger.Info("shutdown started", "timeout", cfg.ShutdownTimeout.String())

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	// Stop new dials first, so no call is claimed while HTTP drains.
	if worker != nil {
		worker.Stop()
	}
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		if worker != nil {
			worker.Abort()
		}
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		if worker != nil {
			worker.Abort()
		}
		return fmt.Errorf("serve: %w", err)
	}

	// An in-flight dial finishes within the deadline. Past the deadline it is
	// aborted. An aborted dial is recorded as dial_unconfirmed, so the next
	// start fails it rather than redialing.
	select {
	case err := <-workerDone:
		if err != nil {
			logger.Error("call worker stopped with error", "error", err.Error())
		}
	case <-shutdownCtx.Done():
		if worker != nil {
			worker.Abort()
		}
		logger.Warn("call worker did not drain before the shutdown timeout")
	}

	// Best-effort: end live legs so callers are not left on a dead stream. It
	// runs under the same deadline, and a failure is logged rather than fatal.
	if worker != nil && legResolver != nil {
		if err := worker.HangupLive(shutdownCtx, legResolver); err != nil {
			logger.Warn("hangup at shutdown incomplete", "error", err.Error())
		}
	}

	logger.Info("shutdown complete")
	return nil
}
