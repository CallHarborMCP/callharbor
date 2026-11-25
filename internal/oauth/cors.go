package oauth

import "net/http"

const corsPreflightMaxAge = "600"

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
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Protocol-Version")
		w.Header().Set("Access-Control-Max-Age", corsPreflightMaxAge)
		w.Header().Add("Vary", "Access-Control-Request-Headers")
		w.WriteHeader(http.StatusNoContent)
	})
}
