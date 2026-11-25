package oauth

import "net/http"

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

// RegisterMCP mounts the protected MCP endpoint. Only POST and the CORS
// preflight are routed. Any other method on /mcp, including GET and DELETE,
// gets 405 from the mux, because the server is stateless and has no SSE stream
// or session to open or close.
//
// The transport is built by wrap, which must apply the bearer middleware this
// package supplies, so the transport only ever sees an authenticated request.
// Preflight OPTIONS is answered without credentials, as browsers send none.
// The CORS origin is "*" but credentials are never allowed, so the wildcard
// grants no ambient authority.
func (s *Server) RegisterMCP(mux *http.ServeMux, wrap MCPWrapper) {
	protected := limitBody(MCPBodyLimit, wrap(s.RequireBearer(ScopeRead), s.protectedResourceMetadataURL()))
	mux.Handle("OPTIONS /mcp", corsMCP(nil))
	mux.Handle("POST /mcp", corsMCP(protected))
}

// MCPBodyLimit bounds one request body on /mcp. This is the only body limit on
// the route: the SDK receives the same value in mcp.StreamableHTTPOptions, so
// the transport cannot apply a different limit to a request this layer admits.
const MCPBodyLimit = 1 << 20

// limitBody caps the request body. A read past the limit fails, so an oversized
// request is never decoded or processed. The response is 413 when the body
// is read before the handler writes anything.
func limitBody(n int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, n)
		}
		next.ServeHTTP(w, r)
	})
}
