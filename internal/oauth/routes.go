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

// RegisterMCP mounts the protected MCP endpoint. POST and the CORS preflight
// are routed to the transport. GET is routed only to a 405 answer, and Go's
// mux also serves HEAD through that GET route. DELETE and other methods have
// no route, so the mux answers 405 for them, and it is not the SPA catch-all.
//
// The GET answer exists because "GET /" is the web UI catch-all in
// internal/server. Without an explicit GET /mcp route, a GET would be served
// as a page, or as 503 when the UI is not built. The server is stateless and
// opens no SSE stream, so a GET has nothing to return; the Streamable HTTP
// spec requires 405 in that case.
//
// The transport is built by wrap, which must apply the bearer middleware this
// package supplies, so the transport only ever sees an authenticated request.
// The 405 answer runs before authentication, so it needs no token. Preflight
// OPTIONS is answered without credentials, as browsers send none. The CORS
// origin is "*" but credentials are never allowed, so the wildcard grants no
// ambient authority.
func (s *Server) RegisterMCP(mux *http.ServeMux, wrap MCPWrapper) {
	protected := limitBody(MCPBodyLimit, wrap(s.RequireBearer(ScopeRead), s.protectedResourceMetadataURL()))
	mux.Handle("OPTIONS /mcp", corsMCP(nil))
	mux.Handle("POST /mcp", corsMCP(protected))
	// GET is routed to the 405 answer. Go's mux also sends HEAD to a GET
	// pattern, so HEAD is covered by the same route.
	mux.Handle("GET /mcp", corsMCP(mcpMethodNotAllowed()))
	// Other methods are listed explicitly. A method-less "/mcp" pattern would
	// conflict with the "GET /" catch-all in internal/server and panic at
	// startup, so each method the endpoint must refuse is named here.
	for _, method := range mcpRefusedMethods {
		mux.Handle(method+" /mcp", corsMCP(mcpMethodNotAllowed()))
	}
}

// mcpRefusedMethods are the methods, besides GET, that /mcp refuses with 405.
// DELETE is included because the server keeps no session to close.
var mcpRefusedMethods = []string{
	http.MethodDelete,
	http.MethodPut,
	http.MethodPatch,
}

// mcpAllowedMethods is the Allow value for /mcp. OPTIONS is listed because it
// is the CORS preflight the endpoint answers.
const mcpAllowedMethods = "POST, OPTIONS"

// mcpMethodNotAllowed answers 405 with the Allow header the Streamable HTTP
// spec expects. It runs before authentication, so a refused method never gets a
// 401 challenge.
func mcpMethodNotAllowed() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", mcpAllowedMethods)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
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
