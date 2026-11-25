package oauth

import "net/http"

const corsPreflightMaxAge = "600"

// corsMCPAllowHeaders is the explicit request-header list for /mcp. It is not
// "*": the Fetch standard does not let a wildcard cover Authorization, and the
// bearer token must be allowed. Each header is listed by name:
//   - Authorization carries the bearer token.
//   - Content-Type carries the JSON-RPC body.
//   - Mcp-Protocol-Version is the negotiated protocol version.
//   - Mcp-Method and Mcp-Name are the standard routing headers the SDK
//     validates for protocol versions from 2026-07-28 on.
//
// Mcp-Param-* headers are not allowed. They are sent only for tools with an
// x-mcp-header annotation, and no tool in api/mcp/tools.json has one. Allowing
// the prefix would widen the list for no caller, so it is added only when a
// tool needs it.
const corsMCPAllowHeaders = "Authorization, Content-Type, Mcp-Protocol-Version, Mcp-Method, Mcp-Name"

// corsPublic adds a wildcard origin to responses from endpoints that remote,
// browser-hosted MCP clients call directly: the metadata documents, the token,
// register and revoke endpoints. It never sends credentials, so the wildcard
// grants no ambient authority. A nil next is valid for a preflight-only route,
// because OPTIONS is answered here and never reaches next.
func corsPublic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method != http.MethodOptions {
			if next != nil {
				next.ServeHTTP(w, r)
			}
			return
		}
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", corsPreflightMaxAge)
		w.Header().Add("Vary", "Access-Control-Request-Headers")
		w.WriteHeader(http.StatusNoContent)
	})
}

// corsMCP is the CORS policy for /mcp. The endpoint is stateless and accepts
// POST only, so it allows only POST and exposes only the challenge header that
// a browser client needs after a 401. Credentials are never allowed.
func corsMCP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Expose-Headers", "WWW-Authenticate")
		if r.Method != http.MethodOptions {
			if next != nil {
				next.ServeHTTP(w, r)
			}
			return
		}
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", corsMCPAllowHeaders)
		w.Header().Set("Access-Control-Max-Age", corsPreflightMaxAge)
		w.Header().Add("Vary", "Access-Control-Request-Headers")
		w.WriteHeader(http.StatusNoContent)
	})
}
