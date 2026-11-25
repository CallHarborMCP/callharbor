package oauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/zitadel/oidc/v3/pkg/oidc"
)

const (
	cimdTimeout    = 5 * time.Second
	cimdMaxBody    = 64 << 10
	cimdCacheSize  = 256
	cimdDefaultTTL = 5 * time.Minute
	cimdMaxTTL     = time.Hour
	// cimdFailTTL bounds how long a failed fetch is remembered. A client that
	// keeps sending a bad URL must not make every request a fresh outbound fetch.
	cimdFailTTL      = 30 * time.Second
	cimdMaxNameRunes = 200
	cimdMaxURIChars  = 2000
	// cimdFailedCap bounds the negative cache. Failures are keyed by the
	// client-supplied URL, so the map must not grow with attacker-chosen IDs.
	cimdFailedCap = 1024
	// cimdMaxConcurrentFetches bounds outbound metadata fetches across all
	// clients. Each one holds a connection and a goroutine for up to cimdTimeout.
	cimdMaxConcurrentFetches = 8
	// maxCIMDClientIDBytes matches the oauth_clients.client_id CHECK. A longer
	// URL is refused before it can be fetched or cached.
	maxCIMDClientIDBytes = 2048
)

// cimdOptions holds test-only overrides. Production code always uses the zero
// value, which applies the default SSRF policy and the system root CAs. Only
// unexported constructors set it, so no exported or environment switch exists.
type cimdOptions struct {
	allowPrivate bool
	rootCAs      *x509.CertPool
}

// cimdMetadata is the subset of a Client ID Metadata Document this server reads.
type cimdMetadata struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

// isCIMDClientID reports whether a client_id is a metadata-document URL: https,
// a non-root path, and no fragment, userinfo or query. A host containing '%'
// is refused: that is an IPv6 zone (fe80::1%eth0, percent-encoded as %25),
// which the dial policy cannot judge reliably.
func isCIMDClientID(raw string) bool {
	if len(raw) > maxCIMDClientIDBytes {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || strings.Contains(u.Host, "%") {
		return false
	}
	return u.Path != "" && u.Path != "/" && u.Fragment == "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery
}

// cimdResolver fetches and validates metadata documents. It is safe for
// concurrent use.
type cimdResolver struct {
	client *http.Client
	clock  Clock

	// mirror persists a fetched client before it is cached or returned. A
	// refused mirror fails the flight, so no caller holds a client with no row.
	mirror func(context.Context, Client) error
	// slots bounds outbound fetches across all clients. A flight takes one
	// before it dials and returns it when the fetch ends.
	slots chan struct{}

	mu    sync.Mutex
	cache map[string]cimdEntry
	order []string // insertion order, oldest first, for eviction
	// failed holds recent fetch failures, so a bad URL is not refetched on
	// every request. It is swept of expired entries and capped at cimdFailedCap.
	failed map[string]time.Time
	// flights holds the fetch in progress for each URL. Concurrent misses for
	// one URL wait on the same fetch instead of starting their own.
	flights map[string]*cimdFlight
}

type cimdEntry struct {
	client  Client
	expires time.Time
}

// cimdFlight is one in-progress fetch. done closes once client and err are set.
type cimdFlight struct {
	done   chan struct{}
	client Client
	ttl    time.Duration
	err    error
}

// newCIMDResolver builds the resolver. Production passes the zero cimdOptions.
// mirror persists a fetched client; it runs once per successful fetch.
func newCIMDResolver(clock Clock, opts cimdOptions, mirror func(context.Context, Client) error) *cimdResolver {
	policy := realtime.EndpointPolicy{AllowPrivate: opts.allowPrivate}
	dialer := &net.Dialer{Timeout: cimdTimeout, Control: policyControl(policy)}
	tr := &http.Transport{
		Proxy:                 nil, // an environment proxy would bypass the Control check
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   cimdTimeout,
		ResponseHeaderTimeout: cimdTimeout,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: opts.rootCAs},
	}
	return &cimdResolver{
		client: &http.Client{
			Transport: tr,
			Timeout:   cimdTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("oauth: metadata document redirects are not followed")
			},
		},
		clock:   clock,
		mirror:  mirror,
		slots:   make(chan struct{}, cimdMaxConcurrentFetches),
		cache:   make(map[string]cimdEntry),
		failed:  make(map[string]time.Time),
		flights: make(map[string]*cimdFlight),
	}
}

// policyControl adapts an EndpointPolicy to net.Dialer.Control. The check runs
// on the resolved address of every attempt, which defeats DNS rebinding.
func policyControl(p realtime.EndpointPolicy) func(string, string, syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return realtime.ErrSSRFBlocked
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !p.Allowed(ip) {
			return realtime.ErrSSRFBlocked
		}
		return nil
	}
}

// resolve returns the cached or freshly fetched client. fetched reports a cache
// miss that hit the network, so callers can tell a fresh mirror from a hit.
// Failures are returned as errors; the composite resolver maps them to
// ErrUnknownClient.
func (r *cimdResolver) resolve(ctx context.Context, clientID string) (Client, bool, error) {
	now := r.clock()
	r.mu.Lock()
	if e, ok := r.cache[clientID]; ok && now.Before(e.expires) {
		r.mu.Unlock()
		return e.client, false, nil
	}
	if until, ok := r.failed[clientID]; ok && now.Before(until) {
		r.mu.Unlock()
		return Client{}, false, errCIMDRecentlyFailed
	}
	if f, ok := r.flights[clientID]; ok {
		r.mu.Unlock()
		return r.wait(ctx, f, false)
	}
	f := &cimdFlight{done: make(chan struct{})}
	r.flights[clientID] = f
	r.mu.Unlock()

	// The flight runs on its own context, so a caller that gives up does not
	// cancel the fetch that other waiters depend on. The goroutine is bounded
	// by cimdTimeout: both the wait for a fetch slot and the fetch itself run
	// under that one timeout (see fetchLimited).
	go r.run(clientID, f, now) // #nosec G118 -- the flight is shared and must outlive any one caller; its slot wait and fetch both share cimdTimeout
	return r.wait(ctx, f, true)
}

// errCIMDRecentlyFailed reports a URL whose last fetch failed recently.
var errCIMDRecentlyFailed = errors.New("oauth: metadata document recently failed")

// errCIMDBusy reports that no fetch slot freed up within cimdTimeout. It is a
// transient refusal: the URL is not negatively cached for it.
var errCIMDBusy = errors.New("oauth: metadata fetch capacity reached")

// run performs one fetch, mirrors the client, publishes the result to the cache
// or the negative cache, and releases the waiters. The mirror runs before the
// cache write, so a client is cached only once its row exists.
func (r *cimdResolver) run(clientID string, f *cimdFlight, now time.Time) {
	c, ttl, err := r.fetchLimited(clientID)
	if err == nil {
		err = r.mirror(context.Background(), c)
	}
	r.mu.Lock()
	delete(r.flights, clientID)
	if err != nil && !errors.Is(err, errCIMDBusy) {
		r.recordFailure(clientID, now.Add(cimdFailTTL))
	}
	r.mu.Unlock()
	if err == nil {
		r.store(clientID, c, now.Add(ttl))
	}
	f.client, f.ttl, f.err = c, ttl, err
	close(f.done)
}

// fetchLimited runs one fetch while holding a fetch slot. Waiting for a slot
// shares the cimdTimeout budget, so a flight never outlives one timeout period.
func (r *cimdResolver) fetchLimited(clientID string) (Client, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cimdTimeout)
	defer cancel()
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return Client{}, 0, errCIMDBusy
	}
	defer func() { <-r.slots }()
	return r.fetch(ctx, clientID)
}

// recordFailure remembers a failed URL until expires. Callers hold mu. Every
// insert first sweeps expired entries. If the map is still at the cap, the
// entry that expires soonest is dropped, so a flood of distinct URLs cannot
// grow it past cimdFailedCap. The sweep is linear in the cap, which is small.
func (r *cimdResolver) recordFailure(clientID string, expires time.Time) {
	now := r.clock()
	for k, until := range r.failed {
		if !now.Before(until) {
			delete(r.failed, k)
		}
	}
	for len(r.failed) >= cimdFailedCap {
		var soonestKey string
		var soonest time.Time
		first := true
		for k, until := range r.failed {
			if first || until.Before(soonest) {
				soonestKey, soonest, first = k, until, false
			}
		}
		delete(r.failed, soonestKey)
	}
	r.failed[clientID] = expires
}

// wait blocks until the flight finishes or ctx ends. fetched is true only for
// the caller that started the flight, so the mirror runs once per fetch.
func (r *cimdResolver) wait(ctx context.Context, f *cimdFlight, fetched bool) (Client, bool, error) {
	select {
	case <-f.done:
		return f.client, fetched && f.err == nil, f.err
	case <-ctx.Done():
		return Client{}, false, ctx.Err()
	}
}

func (r *cimdResolver) store(clientID string, c Client, expires time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.cache[clientID]; !ok {
		if len(r.order) >= cimdCacheSize {
			oldest := r.order[0]
			r.order = r.order[1:]
			delete(r.cache, oldest)
		}
		r.order = append(r.order, clientID)
	}
	r.cache[clientID] = cimdEntry{client: c, expires: expires}
}

// fetch downloads and validates one metadata document. It returns the client
// and the cache TTL taken from Cache-Control max-age.
func (r *cimdResolver) fetch(ctx context.Context, clientID string) (Client, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, cimdTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return Client{}, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return Client{}, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Client{}, 0, fmt.Errorf("oauth: metadata status %d", resp.StatusCode)
	}
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return Client{}, 0, errors.New("oauth: metadata content type is not application/json")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxBody+1))
	if err != nil {
		return Client{}, 0, err
	}
	if len(body) > cimdMaxBody {
		return Client{}, 0, errors.New("oauth: metadata document too large")
	}
	var doc cimdMetadata
	if err := json.Unmarshal(body, &doc); err != nil {
		return Client{}, 0, err
	}
	c, err := validateCIMD(clientID, doc)
	if err != nil {
		return Client{}, 0, err
	}
	return c, cacheTTL(resp.Header.Get("Cache-Control")), nil
}

// validateCIMD applies the server's rules to a parsed document. The error text
// is for logs only; callers see ErrUnknownClient.
func validateCIMD(clientID string, doc cimdMetadata) (Client, error) {
	if doc.ClientID != clientID {
		return Client{}, errors.New("oauth: metadata client_id does not match its URL")
	}
	if len(doc.RedirectURIs) == 0 {
		return Client{}, errors.New("oauth: metadata has no redirect_uris")
	}
	for _, uri := range doc.RedirectURIs {
		if !safeRedirectURI(uri) {
			return Client{}, errors.New("oauth: metadata redirect_uri is not allowed")
		}
	}
	if doc.TokenEndpointAuthMethod != "" && doc.TokenEndpointAuthMethod != string(oidc.AuthMethodNone) {
		return Client{}, errors.New("oauth: metadata auth method must be none")
	}
	if err := checkGrantTypes(doc.GrantTypes); err != nil {
		return Client{}, err
	}
	if err := checkResponseTypes(doc.ResponseTypes); err != nil {
		return Client{}, err
	}
	if utf8.RuneCountInString(doc.ClientName) > cimdMaxNameRunes {
		return Client{}, errors.New("oauth: metadata client_name too long")
	}
	if len(doc.ClientURI) > cimdMaxURIChars {
		return Client{}, errors.New("oauth: metadata client_uri too long")
	}
	return Client{
		ID:               clientID,
		Name:             doc.ClientName,
		URI:              doc.ClientURI,
		RedirectURIs:     append([]string(nil), doc.RedirectURIs...),
		GrantTypes:       append([]string(nil), doc.GrantTypes...),
		MetadataDocument: true,
	}, nil
}

// checkGrantTypes accepts an absent list, or a list drawn from
// {authorization_code, refresh_token} that includes authorization_code.
func checkGrantTypes(grants []string) error {
	if len(grants) == 0 {
		return nil
	}
	hasCode := false
	for _, g := range grants {
		switch g {
		case "authorization_code":
			hasCode = true
		case "refresh_token":
		default:
			return errors.New("oauth: grant_type not supported")
		}
	}
	if !hasCode {
		return errors.New("oauth: grant_types must include authorization_code")
	}
	return nil
}

// checkResponseTypes accepts an absent list or exactly [code].
func checkResponseTypes(types []string) error {
	if len(types) > 0 && (len(types) != 1 || types[0] != "code") {
		return errors.New("oauth: response_types must be [code]")
	}
	return nil
}

// cacheTTL reads max-age from a Cache-Control header, clamped to [0, 1h].
// A missing or unparsable value gives the 5-minute default.
func cacheTTL(cc string) time.Duration {
	for _, part := range strings.Split(cc, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || !strings.EqualFold(k, "max-age") {
			continue
		}
		secs, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return cimdDefaultTTL
		}
		ttl := time.Duration(secs) * time.Second
		return min(max(ttl, 0), cimdMaxTTL)
	}
	return cimdDefaultTTL
}
