package oauth

import (
	"encoding/json"
	"net/http"

	"github.com/zitadel/oidc/v3/pkg/oidc"
)

// Metadata is the RFC 8414 document. It embeds the library's
// DiscoveryConfiguration and adds the RFC 9207 field that library does not
// define.
type Metadata struct {
	oidc.DiscoveryConfiguration
	// AuthorizationResponseIssParameterSupported advertises that every
	// authorization response carries iss (RFC 9207).
	AuthorizationResponseIssParameterSupported bool `json:"authorization_response_iss_parameter_supported"`
}

// AuthorizationServerMetadata returns the metadata document. Every field is a
// value this server actually implements. The document intentionally omits
// openid, id_token response types and any signing algorithms, because none
// are supported.
//
// The registration_endpoint and client_id_metadata_document_supported fields
// belong to chunk B and are added there, so they are not advertised yet.
func (s *Server) AuthorizationServerMetadata() Metadata {
	issuer := s.cfg.Issuer()
	return Metadata{AuthorizationResponseIssParameterSupported: true, DiscoveryConfiguration: oidc.DiscoveryConfiguration{
		Issuer:                issuer,
		AuthorizationEndpoint: issuer + "/oauth/authorize",
		TokenEndpoint:         issuer + "/oauth/token",
		RevocationEndpoint:    issuer + "/oauth/revoke",
		ScopesSupported:       []string{ScopeRead, ScopeCall},
		ResponseTypesSupported: []string{
			string(oidc.ResponseTypeCode),
		},
		GrantTypesSupported: []oidc.GrantType{
			oidc.GrantTypeCode,
			oidc.GrantTypeRefreshToken,
		},
		CodeChallengeMethodsSupported: []oidc.CodeChallengeMethod{
			oidc.CodeChallengeMethodS256,
		},
		TokenEndpointAuthMethodsSupported: []oidc.AuthMethod{
			oidc.AuthMethodNone,
		},
		RevocationEndpointAuthMethodsSupported: []oidc.AuthMethod{
			oidc.AuthMethodNone,
		},
	}}
}

// handleMetadata serves RFC 8414 metadata at both well-known paths. The
// openid-configuration alias is served only because it returns the same
// honest document; no OIDC-only fields are added to it.
func (s *Server) handleMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(s.AuthorizationServerMetadata())
}
