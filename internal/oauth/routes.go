package oauth

import "net/http"

// MCPBodyLimit bounds one request body on /mcp.
const MCPBodyLimit = 1 << 20

// Register mounts the protocol endpoints on mux. OAuth endpoints that browsers
// or remote clients call are CORS-enabled only where listed in cors.go. The
// /api consent endpoints are mounted by RegisterConsentAPI.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /oauth/authorize", s.handleAuthorize)
	mux.Handle("POST /oauth/token", corsPublic(http.HandlerFunc(s.handleToken)))
	mux.Handle("POST /oauth/revoke", corsPublic(http.HandlerFunc(s.handleRevoke)))
	mux.Handle("POST /oauth/register", corsPublic(http.HandlerFunc(s.handleRegister)))
	mux.Handle("OPTIONS /oauth/token", corsPublic(nil))
	mux.Handle("OPTIONS /oauth/revoke", corsPublic(nil))
	mux.Handle("OPTIONS /oauth/register", corsPublic(nil))
	mux.Handle("GET /.well-known/oauth-authorization-server", corsPublic(http.HandlerFunc(s.handleMetadata)))
	mux.Handle("GET /.well-known/oauth-protected-resource/mcp", corsPublic(http.HandlerFunc(s.handleProtectedResourceMetadata)))
	mux.Handle("GET /.well-known/oauth-protected-resource", corsPublic(http.HandlerFunc(s.handleProtectedResourceMetadata)))
}

// MCPWrapper attaches the SDK-facing authentication to the transport. It
// receives the bearer middleware and the protected-resource metadata URL and
// returns the complete protected handler. internal/mcp implements it, which
// keeps this package free of an mcp import.
type MCPWrapper func(bearer func(http.Handler) http.Handler, resourceMetadataURL string) http.Handler

// RegisterMCP mounts the protected MCP endpoint. The transport is built by
// wrap, which must apply the bearer middleware this package supplies, so the
// transport only ever sees an authenticated request. Method and path matching
// is limited to /mcp, so every other path keeps its existing 404 behavior.
//
// Preflight OPTIONS is answered without credentials, as browsers send none.
// The CORS origin is "*" but credentials are never allowed, so the wildcard
// grants no ambient authority.
func (s *Server) RegisterMCP(mux *http.ServeMux, wrap MCPWrapper) {
	protected := limitBody(MCPBodyLimit, wrap(s.RequireBearer(ScopeRead), s.protectedResourceMetadataURL()))
	mux.Handle("OPTIONS /mcp", corsMCP(nil))
	mux.Handle("POST /mcp", corsMCP(protected))
	mux.Handle("GET /mcp", corsMCP(protected))
	mux.Handle("DELETE /mcp", corsMCP(protected))
}

// limitBody caps the request body. Reads past the limit fail, and the
// transport reports an error instead of processing a truncated request.
func limitBody(n int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, n)
		}
		next.ServeHTTP(w, r)
	})
}
