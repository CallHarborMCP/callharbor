// Package mcp exposes the shared service layer as a Model Context Protocol
// server over stateless Streamable HTTP with JSON responses. Every request
// must carry an OAuth access token; the OAuth principal becomes the service
// Actor, and no user or owner identifier is ever read from tool input.
package mcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CallHarborMCP/callharbor/internal/oauth"
	"github.com/CallHarborMCP/callharbor/internal/service"
)

// ServerName is the implementation name reported during initialization.
const ServerName = "callharbor"

// errMissingPrincipal means the bearer middleware accepted a request but left
// no principal in the context. That is a wiring bug, not a client error, so it
// surfaces as a server error and is logged.
var errMissingPrincipal = errors.New("mcp: bearer middleware did not attach a principal")

// errIncompletePrincipal means a principal was attached but has no user or no
// expiry. It is a wiring bug like errMissingPrincipal, and is never a 401.
var errIncompletePrincipal = errors.New("mcp: bearer middleware attached an incomplete principal")

// Handler is the protected MCP endpoint. Its *mcp.Server is built once in New,
// so a bad tool schema panics at startup rather than on the first request.
type Handler struct {
	logger *slog.Logger
	srv    *mcp.Server
	inner  http.Handler
}

// New builds the MCP endpoint. version is reported to clients; an empty value
// becomes "dev". New panics if a tool schema is invalid.
func New(svc *service.Service, version string, logger *slog.Logger) *Handler {
	if version == "" {
		version = "dev"
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: version}, &mcp.ServerOptions{
		Logger: logger,
	})
	registerTools(srv, svc, logger)
	h := &Handler{logger: logger, srv: srv}
	h.inner = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return h.srv
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// DisableLocalhostProtection turns off the SDK's DNS-rebinding check.
		// That check rejects a request whose Host is not loopback when the
		// listener is loopback, which is exactly the shape of a same-host
		// reverse proxy serving a public origin, and it would refuse every
		// legitimate remote client. The protection it adds is unnecessary
		// here: every request must carry an OAuth bearer token, which a
		// rebinding page cannot obtain, because the token is bound to this
		// origin and is never sent by a cross-origin page.
		DisableLocalhostProtection: true,
		// The same limit the oauth route applies in front of this handler.
		// The SDK enforces it again on the read, so the transport can never
		// decode a larger body than the route admits.
		MaxRequestBodyBytes: oauth.MCPBodyLimit,
		Logger:              logger,
	})
	return h
}

// ServeHTTP makes Handler usable directly. Authentication is applied by Wrap.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.inner.ServeHTTP(w, r)
}

// Wrap returns the handler protected by the OAuth bearer check. The verifier
// maps the OAuth principal to the SDK's TokenInfo, so the SDK can pass it to
// tool handlers through RequestExtra. bearer is the middleware that validates
// the token; this method never accepts a token itself.
func (h *Handler) Wrap(bearer func(http.Handler) http.Handler, resourceMetadataURL string) http.Handler {
	verify := func(ctx context.Context, token string, req *http.Request) (*auth.TokenInfo, error) {
		return verifyPrincipal(ctx, h.logger, token, req)
	}
	verified := auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: resourceMetadataURL,
	})
	return bearer(verified(h))
}

// verifyPrincipal reads the principal the OAuth middleware already validated
// and maps it to TokenInfo. It never accepts a token itself. The expiry comes
// from the stored grant row, which RequireBearer checked on this request.
func verifyPrincipal(ctx context.Context, logger *slog.Logger, _ string, _ *http.Request) (*auth.TokenInfo, error) {
	p, ok := oauth.PrincipalFromContext(ctx)
	return principalTokenInfo(logger, p, ok)
}

// principalTokenInfo maps a principal to TokenInfo. A missing principal, or one
// without a user or an expiry, is a wiring bug: the bearer middleware must have
// stored a complete principal before the transport runs. It is logged and
// returned as a non-ErrInvalidToken error, so the SDK answers 500. A 401 would
// tell the client to re-authenticate for a server fault, and a 200 would serve
// a request with no identity.
func principalTokenInfo(logger *slog.Logger, p oauth.Principal, ok bool) (*auth.TokenInfo, error) {
	if !ok {
		logWiringFault(logger, "mcp request reached the verifier without a principal", errMissingPrincipal)
		return nil, errMissingPrincipal
	}
	if p.UserID == "" || p.ExpiresAt.IsZero() {
		logWiringFault(logger, "mcp request principal is incomplete", errIncompletePrincipal)
		return nil, errIncompletePrincipal
	}
	return &auth.TokenInfo{
		UserID:     p.UserID,
		Scopes:     p.Scopes,
		Expiration: p.ExpiresAt,
	}, nil
}

func logWiringFault(logger *slog.Logger, msg string, cause error) {
	if logger != nil {
		logger.Error(msg, "cause", cause.Error())
	}
}

// actorFromRequest converts the SDK's TokenInfo into a service Actor. It is
// the only place an Actor is built for MCP, and it fails closed.
func actorFromRequest(req *mcp.CallToolRequest) (service.Actor, error) {
	if req == nil || req.Extra == nil || req.Extra.TokenInfo == nil || req.Extra.TokenInfo.UserID == "" {
		return service.Actor{}, service.AsErrorWith(service.CodeUnauthenticated, "authentication required")
	}
	ti := req.Extra.TokenInfo
	return service.Actor{
		UserID: ti.UserID,
		Scopes: ti.Scopes,
	}, nil
}
