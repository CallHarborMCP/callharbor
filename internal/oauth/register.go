package oauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

const (
	registerMaxBody   = 16 << 10
	registerWindow    = 10 * time.Minute
	registerPerWindow = 20
	// Authorization requests are cheap per call, but each one stores a
	// pending request and can start a metadata fetch, so they get a budget too.
	authorizeWindow     = 1 * time.Minute
	authorizePerWindow  = 60
	registerMaxKeys     = 4096
	registerGlobalCap   = 10000
	registerIDBytes     = 16
	registerClientIDPfx = "cho_client_"
)

// registrationRequest is the RFC 7591 client metadata this server reads. Other
// members are ignored, as RFC 7591 section 2 requires.
type registrationRequest struct {
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	ApplicationType         string   `json:"application_type"`
}

// registrationResponse is the RFC 7591 section 3.2.1 success body.
type registrationResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	ApplicationType         string   `json:"application_type"`
}

// registrationError is the RFC 7591 section 3.2.2 error body.
type registrationError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// registrationLimiter is a per-key fixed-window counter with a bounded key set.
// Keys are client IP addresses taken from RemoteAddr only.
type registrationLimiter struct {
	mu      sync.Mutex
	clock   Clock
	window  time.Duration
	budget  int
	windows map[string]registrationWindowState
}

type registrationWindowState struct {
	start time.Time
	count int
}

// newRegistrationLimiter allows budget events per key in each window. The
// registration and authorization endpoints each have their own limiter.
func newRegistrationLimiter(clock Clock, window time.Duration, budget int) *registrationLimiter {
	return &registrationLimiter{clock: clock, window: window, budget: budget, windows: make(map[string]registrationWindowState)}
}

// allow records one event for key. When the limit is reached it returns
// false and how long until the window resets.
func (l *registrationLimiter) allow(key string) (bool, time.Duration) {
	now := l.clock()
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.windows[key]
	if !ok || !now.Before(w.start.Add(l.window)) {
		if !ok && len(l.windows) >= registerMaxKeys {
			l.evictOldest()
		}
		l.windows[key] = registrationWindowState{start: now, count: 1}
		return true, 0
	}
	if w.count >= l.budget {
		return false, w.start.Add(l.window).Sub(now)
	}
	w.count++
	l.windows[key] = w
	return true, 0
}

// evictOldest drops the key whose window started earliest. Callers hold mu.
func (l *registrationLimiter) evictOldest() {
	var oldestKey string
	var oldest time.Time
	first := true
	for k, w := range l.windows {
		if first || w.start.Before(oldest) {
			oldestKey, oldest, first = k, w.start, false
		}
	}
	delete(l.windows, oldestKey)
}

// registrationClientIP returns the rate-limit key for a request's client.
//
// The peer is the host of RemoteAddr. X-Forwarded-For is read only when the
// peer is in trusted (CALLHARBOR_TRUSTED_PROXIES). Then the chain is walked
// from the right, and the first entry that is not itself trusted is the client.
// A peer outside trusted, a missing header, a malformed or zoned entry, or a
// chain made only of trusted hops all fall back to the peer. X-Real-IP and
// Forwarded are never read. An IPv6 client keys on its /64, since one host
// usually controls a whole /64; an IPv4 client, including an IPv4-mapped one,
// keys on its full address.
func registrationClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	if inTrustedPrefixes(peer, trusted) {
		if client, ok := forwardedClient(r.Header.Values("X-Forwarded-For"), trusted); ok {
			return limiterKey(client)
		}
	}
	return limiterKey(peer)
}

// forwardedClient returns the rightmost forwarded entry that is not a trusted
// proxy. Entries are comma-split across all header values, in order. It
// reports false when the chain has no such entry or any entry it reaches is
// empty, zoned, or not an IP address. Those entries are refused rather than
// skipped: a broken chain from a trusted proxy is a configuration fault, and
// the peer is the conservative key.
func forwardedClient(values []string, trusted []netip.Prefix) (netip.Addr, bool) {
	var hops []string
	for _, v := range values {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop == "" || strings.Contains(hop, "%") {
			return netip.Addr{}, false
		}
		addr, err := netip.ParseAddr(hop)
		if err != nil {
			return netip.Addr{}, false
		}
		addr = addr.Unmap()
		if !inTrustedPrefixes(addr, trusted) {
			return addr, true
		}
	}
	return netip.Addr{}, false
}

// inTrustedPrefixes reports whether addr is inside one of the trusted prefixes.
func inTrustedPrefixes(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// limiterKey is the bucket for a client address: the full address for IPv4, and
// the /64 for IPv6.
func limiterKey(addr netip.Addr) string {
	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.Prefix(64)
	if err != nil {
		return addr.String()
	}
	return prefix.String()
}

// writeRegistrationError writes an RFC 7591 error with status 400, or the
// status given for rate limiting.
func writeRegistrationError(w http.ResponseWriter, status int, code, description string) {
	writeNoStore(w, status, registrationError{Error: code, ErrorDescription: description})
}

// handleRegister serves RFC 7591 dynamic client registration. It creates a
// public client with a random client_id and no secret. Clients created here
// are tagged "dynamic" in storage.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeRegistrationError(w, http.StatusUnsupportedMediaType, "invalid_client_metadata", "Content-Type must be application/json")
		return
	}
	if ok, retry := s.registry.allow(registrationClientIP(r, s.trustedProxies)); !ok {
		w.Header().Set("Retry-After", retryAfterSeconds(retry))
		writeRegistrationError(w, http.StatusTooManyRequests, "invalid_client_metadata", "too many registrations; try again later")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, registerMaxBody+1))
	if err != nil {
		writeRegistrationError(w, http.StatusBadRequest, "invalid_client_metadata", "cannot read request body")
		return
	}
	if len(body) > registerMaxBody {
		writeRegistrationError(w, http.StatusRequestEntityTooLarge, "invalid_client_metadata", "request body too large")
		return
	}
	var req registrationRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRegistrationError(w, http.StatusBadRequest, "invalid_client_metadata", "body must be a JSON object")
		return
	}
	resp, rerr := s.validateRegistration(req)
	if rerr != nil {
		writeRegistrationError(w, http.StatusBadRequest, rerr.code, rerr.description)
		return
	}
	ctx := r.Context()
	// The cap counts every dynamic client ever registered, so the lower bound
	// is the Unix epoch.
	n, err := s.store.CountDynamicOAuthClients(ctx, formatTime(time.Unix(0, 0)))
	if err != nil {
		writeRegistrationError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "registration is unavailable")
		return
	}
	if n >= s.registryCap {
		writeRegistrationError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "registration capacity reached")
		return
	}
	if err := s.persistRegistration(ctx, &resp); err != nil {
		s.logger.Error("oauth: persist dynamic registration", "error", err)
		writeRegistrationError(w, http.StatusInternalServerError, "server_error", "registration failed")
		return
	}
	writeNoStore(w, http.StatusCreated, resp)
}

// registrationFailure is a validation error with its RFC 7591 code.
type registrationFailure struct {
	code        string
	description string
}

// validateRegistration applies the RFC 7591 rules and returns the registered
// metadata, with defaults filled in. The client_id is not yet assigned.
func (s *Server) validateRegistration(req registrationRequest) (registrationResponse, *registrationFailure) {
	fail := func(code, desc string) (registrationResponse, *registrationFailure) {
		return registrationResponse{}, &registrationFailure{code: code, description: desc}
	}
	if len(req.RedirectURIs) == 0 {
		return fail("invalid_redirect_uri", "redirect_uris is required")
	}
	for _, uri := range req.RedirectURIs {
		if !safeRedirectURI(uri) {
			return fail("invalid_redirect_uri", "redirect_uris must be https, or http on loopback, with no fragment or wildcard")
		}
	}
	if req.TokenEndpointAuthMethod != "" && req.TokenEndpointAuthMethod != "none" {
		return fail("invalid_client_metadata", "token_endpoint_auth_method must be none")
	}
	if err := checkGrantTypes(req.GrantTypes); err != nil {
		return fail("invalid_client_metadata", err.Error())
	}
	if err := checkResponseTypes(req.ResponseTypes); err != nil {
		return fail("invalid_client_metadata", err.Error())
	}
	appType := req.ApplicationType
	switch appType {
	case "":
		appType = "web"
	case "web", "native":
	default:
		return fail("invalid_client_metadata", "application_type must be web or native")
	}
	if utf8.RuneCountInString(req.ClientName) > cimdMaxNameRunes {
		return fail("invalid_client_metadata", "client_name is too long")
	}
	if len(req.ClientURI) > cimdMaxURIChars {
		return fail("invalid_client_metadata", "client_uri is too long")
	}
	grants := req.GrantTypes
	if len(grants) == 0 {
		grants = []string{"authorization_code", "refresh_token"}
	}
	responses := req.ResponseTypes
	if len(responses) == 0 {
		responses = []string{"code"}
	}
	return registrationResponse{
		ClientName:              req.ClientName,
		ClientURI:               req.ClientURI,
		RedirectURIs:            req.RedirectURIs,
		TokenEndpointAuthMethod: "none",
		GrantTypes:              grants,
		ResponseTypes:           responses,
		ApplicationType:         appType,
	}, nil
}

// persistRegistration assigns the client_id and stores the client and its
// audit event together. The audit event has no actor: the caller is anonymous.
func (s *Server) persistRegistration(ctx context.Context, resp *registrationResponse) error {
	buf := make([]byte, registerIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	now := s.clock()
	resp.ClientID = registerClientIDPfx + hex.EncodeToString(buf)
	resp.ClientIDIssuedAt = now.Unix()

	uris, err := json.Marshal(resp.RedirectURIs)
	if err != nil {
		return err
	}
	grants, err := json.Marshal(resp.GrantTypes)
	if err != nil {
		return err
	}
	stamp := formatTime(now)
	return s.store.InsertDynamicOAuthClient(ctx, storage.OAuthClient{
		ClientID:     resp.ClientID,
		Source:       "dynamic",
		ClientName:   resp.ClientName,
		ClientURI:    resp.ClientURI,
		RedirectURIs: string(uris),
		GrantTypes:   string(grants),
		CreatedAt:    stamp,
		UpdatedAt:    stamp,
	}, storage.AuditEvent{
		OccurredAt:   stamp,
		ActorRole:    "system",
		Action:       "oauth.client.register",
		ResourceType: "oauth_client",
		ResourceID:   resp.ClientID,
		Outcome:      "success",
		DetailJSON:   "{}",
	})
}

// retryAfterSeconds renders a wait as whole seconds, rounded up, minimum 1.
func retryAfterSeconds(d time.Duration) string {
	secs := int64((d + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return strconv.FormatInt(secs, 10)
}
