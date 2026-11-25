// Package mcp exposes the shared service layer as a Model Context Protocol
// server over stateless Streamable HTTP with JSON responses. Every request
// must carry an OAuth access token; the OAuth principal becomes the service
// Actor, and no user or owner identifier is ever read from tool input.
package mcp

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CallHarborMCP/callharbor/internal/oauth"
	"github.com/CallHarborMCP/callharbor/internal/service"
)

// ServerName is the implementation name reported during initialization.
const ServerName = "callharbor"

// MaxRequestBodyBytes bounds one JSON-RPC request body.
const MaxRequestBodyBytes = 1 << 20

// Handler is the protected MCP endpoint. It is an http.Handler that expects
// the OAuth principal to be attached by the bearer middleware in front of it.
type Handler struct {
	svc     *service.Service
	version string
	logger  *slog.Logger
	inner   http.Handler
}

// New builds the MCP endpoint. version is reported to clients; an empty value
// becomes "dev".
func New(svc *service.Service, version string, logger *slog.Logger) *Handler {
	if version == "" {
		version = "dev"
	}
	h := &Handler{svc: svc, version: version, logger: logger}
	h.inner = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return h.newServer()
	}, &mcp.StreamableHTTPOptions{
		Stateless:           true,
		JSONResponse:        true,
		MaxRequestBodyBytes: MaxRequestBodyBytes,
		Logger:              logger,
	})
	return h
}

// ServeHTTP makes Handler usable directly. Authentication is applied by Wrap.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.inner.ServeHTTP(w, r)
}

// Wrap returns the handler protected by the OAuth bearer check. The
// verifier maps the OAuth principal to the SDK's TokenInfo, so the SDK can
// pass it to tool handlers through RequestExtra.
func (h *Handler) Wrap(bearer func(http.Handler) http.Handler, resourceMetadataURL string) http.Handler {
	verified := auth.RequireBearerToken(verifyPrincipal, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: resourceMetadataURL,
		Scopes:              nil,
	})
	return bearer(verified(h))
}

// verifyPrincipal reads the principal the OAuth middleware already validated
// and maps it to TokenInfo. It never accepts a token itself. The expiry comes
// from the stored grant row, which RequireBearer checked on this request.
func verifyPrincipal(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
	p, ok := oauth.PrincipalFromContext(ctx)
	if !ok || p.UserID == "" || p.ExpiresAt.IsZero() {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{
		UserID:     p.UserID,
		Scopes:     p.Scopes,
		Expiration: p.ExpiresAt,
		Extra: map[string]any{
			"client_id": p.ClientID,
			"grant_id":  p.GrantID,
		},
	}, nil
}

// newServer builds one SDK server with the implemented tools registered.
// Planned operations are not registered at all, so clients cannot call them.
func (h *Handler) newServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: h.version}, &mcp.ServerOptions{
		Logger: h.logger,
	})
	registerTools(srv, h.svc)
	return srv
}

// actorFromRequest converts the SDK's TokenInfo into a service Actor. It is
// the only place an Actor is built for MCP, and it fails closed.
func actorFromRequest(req *mcp.CallToolRequest) (service.Actor, error) {
	if req == nil || req.Extra == nil || req.Extra.TokenInfo == nil || req.Extra.TokenInfo.UserID == "" {
		return service.Actor{}, service.AsErrorWith(service.CodeUnauthenticated, "authentication required")
	}
	ti := req.Extra.TokenInfo
	clientID, _ := ti.Extra["client_id"].(string)
	return service.Actor{
		UserID:   ti.UserID,
		Scopes:   ti.Scopes,
		ClientID: clientID,
		Channel:  service.ChannelMCP,
	}, nil
}
