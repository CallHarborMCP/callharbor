package oauth

import (
	"encoding/json"
	"net/http"
)

// protectedResourceMetadata is the RFC 9728 document for the /mcp resource.
type protectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceName           string   `json:"resource_name"`
}

// handleProtectedResourceMetadata serves RFC 9728 metadata. It is served at the
// path-specific well-known URL and at the root, so either form of discovery works.
func (s *Server) handleProtectedResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(protectedResourceMetadata{
		Resource:               s.cfg.Resource(),
		AuthorizationServers:   []string{s.cfg.Issuer()},
		ScopesSupported:        []string{ScopeRead, ScopeCall},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "CallHarbor",
	})
}

// handleMCPPlaceholder answers POST and GET /mcp. Authorization runs before
// this handler, so a valid token reaches it and receives this honest answer.
// Issue #67 replaces this handler with the MCP transport. Never make it return
// success before that transport exists.
func (s *Server) handleMCPPlaceholder(w http.ResponseWriter, _ *http.Request) {
	writeNoStore(w, http.StatusNotImplemented, map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"error": map[string]any{
			"code":    -32601,
			"message": "MCP transport is not available yet",
		},
	})
}
