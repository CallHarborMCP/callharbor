package oauth

import "net/http"

// Register mounts the chunk A protocol endpoints on mux. The consent REST API,
// the bearer resource-server middleware and /mcp arrive in later chunks; app
// wiring is also chunk C, so nothing mounts this yet.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("POST /oauth/token", s.handleToken)
	mux.HandleFunc("POST /oauth/revoke", s.handleRevoke)
	mux.HandleFunc("POST /oauth/register", s.handleRegister)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleMetadata)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleMetadata)
}
