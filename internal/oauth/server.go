package oauth

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/schema"
)

// timeLayout matches the identity and storage timestamp format so OAuth rows
// sort and compare the same way as the rest of the database.
const timeLayout = "2006-01-02T15:04:05.000Z"

// maxFormBytes bounds every OAuth form body. Requests are small; this stops a
// client from forcing large parses.
const maxFormBytes = 16 << 10

// Server is the OAuth authorization server. It holds no mutable state beyond
// its collaborators, so one instance serves all requests concurrently.
type Server struct {
	cfg     Config
	store   Store
	clients ClientResolver
	clock   Clock
	decoder *schema.Decoder
	// logger records internal failures. It never receives credential values.
	logger *slog.Logger
	// registry limits dynamic registrations per client IP. registryCap is the
	// global ceiling on dynamic clients; tests lower it through newServer.
	registry    *registrationLimiter
	registryCap int
}

// NewServer builds the authorization server. The configured public origin
// must already be normalized by config.Load.
func NewServer(cfg Config, store Store, clients ClientResolver, clock Clock, logger *slog.Logger) *Server {
	return newServer(cfg, store, clients, clock, logger, registerGlobalCap)
}

// newServer is NewServer with an explicit dynamic-client cap.
func newServer(cfg Config, store Store, clients ClientResolver, clock Clock, logger *slog.Logger, registryCap int) *Server {
	dec := schema.NewDecoder()
	dec.IgnoreUnknownKeys(true)
	return &Server{
		cfg: cfg, store: store, clients: clients, clock: clock, decoder: dec, logger: logger,
		registry: newRegistrationLimiter(clock), registryCap: registryCap,
	}
}

// formatTime renders t in the database timestamp layout, in UTC.
func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// errorParams returns the RFC 6749 error response parameters for e, echoing
// the request state when there is one.
func errorParams(e *oidc.Error, state string) url.Values {
	v := url.Values{}
	v.Set("error", errorCode(e))
	if e.Description != "" {
		v.Set("error_description", e.Description)
	}
	if state != "" {
		v.Set("state", state)
	}
	return v
}

// errorCode extracts the RFC 6749 error code from an oidc.Error. The library
// keeps the code in an unexported field, so it is read from its JSON form.
func errorCode(e *oidc.Error) string {
	raw, err := json.Marshal(e)
	if err != nil {
		return "server_error"
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Error == "" {
		return "server_error"
	}
	return body.Error
}

// joinScopes is the space-delimited scope string stored with a grant.
func joinScopes(scopes []string) string { return strings.Join(scopes, " ") }

// writeNoStore writes a JSON body with the caching headers required for every
// token-bearing or error response.
func writeNoStore(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
