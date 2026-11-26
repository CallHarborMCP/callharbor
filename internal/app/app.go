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
func newCallIngress(store *storage.Store, sec *secrets.Service, tokens *callstream.Tokens, legs callworker.LegResolver, cfg config.Config, opts runOptions, logger *slog.Logger) ([]func(*http.ServeMux), *callstream.Media, error) {
	// The public origin must map to a WebSocket origin. Checking it here means
	// StreamURL cannot fail later for the configured origin.
	if _, err := callworker.StreamURL(cfg.PublicURL, "probe", "probe"); err != nil {
		return nil, nil, fmt.Errorf("call stream url: %w", err)
	}
	// The same leg hangup the media path and shutdown use, so a retry is one leg.
	legHangup := func(ctx context.Context, leg storage.LiveCallLeg) error {
		return callworker.HangupLeg(ctx, legs, leg)
	}
	webhooks, err := callstream.NewWebhooks(store, opts.Now, legHangup, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("call webhooks: %w", err)
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
		return nil, nil, fmt.Errorf("call ai resolver: %w", err)
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
		Webhooks: webhooks,
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
	}}, media, nil
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
		Codec: mediaFormat,
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
	var mediaSvc *callstream.Media
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
		callRoutes, mediaSvc, err = newCallIngress(store, secretSvc, tokens, legResolver, cfg, opts, logger)
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

	// Every exit from here goes through stop, so the store never closes under
	// a live media session or a running dial. serveFailure is set when the
	// server stopped on its own, which the caller reports as an error.
	var serveFailure error
	select {
	case err := <-serveErr:
		serveFailure = fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	runErr := shutdown(shutdownParts{
		cfg:        cfg,
		logger:     logger,
		srv:        srv,
		httpServer: httpServer,
		serveErr:   serveErr,
		serveDead:  serveFailure != nil,
		worker:     worker,
		workerDone: workerDone,
		legs:       legResolver,
		media:      mediaSvc,
	})
	if serveFailure != nil {
		return errors.Join(serveFailure, runErr)
	}
	return runErr
}

// shutdownParts is what shutdown needs from run. serveDead is true when the
// HTTP server already stopped, so shutdown must not wait on serveErr again.
type shutdownParts struct {
	cfg        config.Config
	logger     *slog.Logger
	srv        *server.Server
	httpServer *http.Server
	serveErr   chan error
	serveDead  bool
	worker     *callworker.Worker
	workerDone chan error
	legs       callworker.LegResolver
	media      *callstream.Media
}

// shutdown runs on every exit from run: a clean stop, a failed Shutdown and a
// failed server. Its budgets are separate, so a slow drain cannot leave the
// hangup pass with no time:
//
//  1. Stop new work: the worker takes no new dials and media refuses new streams.
//  2. Drain HTTP under the shutdown deadline.
//  3. Let in-flight dials finish under the same deadline, then abort them. An
//     aborted dial is recorded as dial_unconfirmed and never redialed.
//  4. Cancel and wait for media sessions, so none touches the store after close.
//  5. Hang up every live leg under a reserved budget of its own. The budget
//     is a share of the shutdown timeout with a floor, so it runs even when the
//     drain used the whole deadline.
//
// The store is closed by run after shutdown returns, and only when every
// media session and dial has released.
func shutdown(p shutdownParts) error {
	p.srv.SetReady(false)
	p.logger.Info("shutdown started", "timeout", p.cfg.ShutdownTimeout.String())

	var drainErr error
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), p.cfg.ShutdownTimeout)
	defer cancelDrain()

	if p.worker != nil {
		p.worker.Stop()
	}
	if p.media != nil {
		// Refuse new streams and cancel running ones now. Their local ends are
		// suppressed; HangupLive below ends every live leg.
		p.media.StopMedia()
		// Refuse new orphan hangups from webhooks at the same point. Running
		// ones are waited for below, before the store closes.
		p.media.RefuseOrphanHangups()
	}
	if !p.serveDead {
		if err := p.httpServer.Shutdown(drainCtx); err != nil {
			drainErr = fmt.Errorf("shutdown: %w", err)
		}
		if err := <-p.serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) && drainErr == nil {
			drainErr = fmt.Errorf("serve: %w", err)
		}
	}

	if p.worker != nil {
		select {
		case err := <-p.workerDone:
			if err != nil {
				p.logger.Error("call worker stopped with error", "error", err.Error())
			}
		case <-drainCtx.Done():
			// Past the deadline: abort the dial, which records it as
			// dial_unconfirmed. Then wait for the worker to return, bounded, so
			// it does not write to the store after the store closes.
			p.logger.Warn("call worker did not drain before the shutdown timeout")
			p.worker.Abort()
			select {
			case err := <-p.workerDone:
				if err != nil {
					p.logger.Error("call worker stopped with error", "error", err.Error())
				}
			case <-time.After(hangupFloor):
				p.logger.Warn("call worker did not stop before the store closed")
			}
		}
	}

	if p.media != nil && !p.media.WaitMedia(mediaWaitBound(p.cfg.ShutdownTimeout)) {
		p.logger.Warn("media sessions did not finish before the store closed")
	}
	// Orphan hangups were refused from StopMedia onward. Wait here for any
	// already running, so none touches the store after close.
	if p.media != nil {
		orphanCtx, cancelOrphan := context.WithTimeout(context.Background(), mediaWaitBound(p.cfg.ShutdownTimeout))
		if err := p.media.WaitOrphanHangups(orphanCtx); err != nil {
			p.logger.Warn("orphan hangups did not finish before the store closed")
		}
		cancelOrphan()
	}

	// Best-effort: end live legs so callers are not left on a dead stream. The
	// budget is reserved, not the leftover of the drain: a drain that used the
	// whole deadline must not skip the hangups.
	if p.worker != nil && p.legs != nil {
		hangupCtx, cancelHangup := context.WithTimeout(context.Background(), hangupBudget(p.cfg.ShutdownTimeout))
		if err := p.worker.HangupLive(hangupCtx, p.legs); err != nil {
			p.logger.Warn("hangup at shutdown incomplete", "error", err.Error())
		}
		cancelHangup()
	}

	if drainErr != nil {
		return drainErr
	}
	p.logger.Info("shutdown complete")
	return nil
}

// hangupFloor is the least time reserved for the shutdown hangup pass, so a
// short shutdown timeout still gives every live leg one attempt.
const hangupFloor = 5 * time.Second

// hangupBudget reserves a share of the shutdown timeout for the hangup pass.
func hangupBudget(timeout time.Duration) time.Duration {
	if b := timeout / 3; b > hangupFloor {
		return b
	}
	return hangupFloor
}

// mediaWaitBound bounds how long a media session may take to finish after
// cancellation before the store closes anyway.
func mediaWaitBound(timeout time.Duration) time.Duration {
	return hangupBudget(timeout)
}
