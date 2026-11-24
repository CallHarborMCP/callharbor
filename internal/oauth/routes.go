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
	mux.Handle("GET /.well-known/openid-configuration", corsPublic(http.HandlerFunc(s.handleMetadata)))
	mux.Handle("GET /.well-known/oauth-protected-resource/mcp", corsPublic(http.HandlerFunc(s.handleProtectedResourceMetadata)))
	mux.Handle("GET /.well-known/oauth-protected-resource", corsPublic(http.HandlerFunc(s.handleProtectedResourceMetadata)))

	// The /mcp placeholder is protected now. Issue #67 replaces the handler.
	// CORS for /mcp is deliberately absent and belongs to #67.
	bearer := s.RequireBearer(ScopeRead)
	mux.Handle("POST /mcp", bearer(http.HandlerFunc(s.handleMCPPlaceholder)))
	mux.Handle("GET /mcp", bearer(http.HandlerFunc(s.handleMCPPlaceholder)))
}
