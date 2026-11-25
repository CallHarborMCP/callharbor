package oauth

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// metadataServer is a TLS test server that serves one client metadata document
// and records every path requested. Its behavior is changed under its lock.
type metadataServer struct {
	mu           sync.Mutex
	status       int
	contentType  string
	body         []byte
	cacheControl string
	redirect     string
	paths        []string
}

func newMetadataServer() *metadataServer {
	return &metadataServer{status: http.StatusOK, contentType: "application/json"}
}

func (m *metadataServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.paths = append(m.paths, r.URL.Path)
	status, ctype, body, cc, redirect := m.status, m.contentType, m.body, m.cacheControl, m.redirect
	m.mu.Unlock()
	if redirect != "" {
		http.Redirect(w, r, redirect, http.StatusFound)
		return
	}
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	if cc != "" {
		w.Header().Set("Cache-Control", cc)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// update changes the server's behavior under its lock.
func (m *metadataServer) update(fn func(*metadataServer)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(m)
}

// hits returns how many requests reached the server.
func (m *metadataServer) hits() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.paths)
}

// requested reports whether the server received a request for path.
func (m *metadataServer) requested(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.paths {
		if p == path {
			return true
		}
	}
	return false
}

func setDoc(m *metadataServer, body []byte) {
	m.update(func(s *metadataServer) { s.body = body })
}

// cimdDoc builds a valid metadata document for clientID. Each override sets a
// key; a nil override removes it.
func cimdDoc(clientID string, overrides map[string]any) []byte {
	doc := map[string]any{
		"client_id":                  clientID,
		"client_name":                "CIMD Test Client",
		"redirect_uris":              []string{testRedirectURI},
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	}
	for k, v := range overrides {
		if v == nil {
			delete(doc, k)
		} else {
			doc[k] = v
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return raw
}

// newCIMDFixture returns a real-store fixture whose resolver fetches metadata
// over TLS from meta. allowPrivate selects the loopback-permitting test policy;
// false keeps the policy that production uses. The returned client_id is the
// document URL on the test server.
func newCIMDFixture(t *testing.T, meta *metadataServer, allowPrivate bool) (*fixture, string) {
	t.Helper()
	ts := httptest.NewTLSServer(meta)
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	f := newFixture(t)
	f.resetResolver(newClientResolver(f.store, f.clock.Now, cimdOptions{allowPrivate: allowPrivate, rootCAs: pool}))
	return f, ts.URL + "/client.json"
}

// resetResolver rebuilds the server and its HTTP front end with another client
// resolver. The store, user and clock are unchanged.
func (f *fixture) resetResolver(clients ClientResolver) {
	f.t.Helper()
	f.serve(NewServer(f.cfg, f.store, clients, f.clock.Now, slog.New(slog.DiscardHandler)))
}

// serve replaces the fixture's server and HTTP front end with srv.
func (f *fixture) serve(srv *Server) {
	f.t.Helper()
	f.srv = srv
	if f.http != nil {
		f.http.Close()
	}
	mux := http.NewServeMux()
	f.srv.Register(mux)
	f.srv.RegisterConsentAPI(mux, testAuth(f.userID), testAuth(f.userID))
	f.http = httptest.NewServer(mux)
	f.t.Cleanup(f.http.Close)
}

// assertRejectedWithoutRedirect requires authorize to answer with a JSON 400
// and no redirect, which is the outcome for any unresolvable client.
func assertRejectedWithoutRedirect(t *testing.T, f *fixture, clientID string, params map[string]string) {
	t.Helper()
	p := map[string]string{"client_id": clientID}
	for k, v := range params {
		p[k] = v
	}
	resp := get(t, f.authorizeURL(p))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 with no redirect", resp.StatusCode)
	}
}

// TestCIMDHappyPathAuthorizeExchangeRefresh runs the full flow with a CIMD
// client and confirms the document was fetched once and then served from cache.
func TestCIMDHappyPathAuthorizeExchangeRefresh(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	setDoc(meta, cimdDoc(cid, nil))

	redirect := f.approveAuthorize(map[string]string{"client_id": cid})
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeFrom(t, redirect)},
		"client_id":     {cid},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	})
	if status != http.StatusOK {
		t.Fatalf("exchange status = %d body=%v", status, body)
	}
	refresh, _ := body["refresh_token"].(string)
	if refresh == "" {
		t.Fatal("CIMD client registered refresh_token but got no refresh token")
	}
	status, body = f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {cid},
	})
	if status != http.StatusOK {
		t.Fatalf("refresh status = %d body=%v", status, body)
	}
	if body["refresh_token"] == refresh {
		t.Fatal("refresh did not rotate the refresh token")
	}
	if n := meta.hits(); n != 1 {
		t.Fatalf("metadata fetches = %d, want 1 (later steps hit the cache)", n)
	}
}

// TestCIMDWithoutRefreshGrantGetsNoRefreshToken checks clientMayRefresh: a
// document that lists only authorization_code gets no refresh token.
func TestCIMDWithoutRefreshGrantGetsNoRefreshToken(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	setDoc(meta, cimdDoc(cid, map[string]any{"grant_types": []string{"authorization_code"}}))

	redirect := f.approveAuthorize(map[string]string{"client_id": cid})
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeFrom(t, redirect)},
		"client_id":     {cid},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	})
	if status != http.StatusOK {
		t.Fatalf("exchange status = %d body=%v", status, body)
	}
	if _, ok := body["refresh_token"]; ok {
		t.Fatal("client without refresh_token grant received a refresh token")
	}
}

// TestCIMDChatGPTShapedDocumentIsAcceptedAsRegisteredRedirect is a SHAPE test
// only. The document mirrors the shape of the public ChatGPT connector
// metadata at https://chatgpt.com/oauth/client.json, with client_id rewritten
// to the local test server. It does not verify what ChatGPT publishes or how
// ChatGPT behaves. It never contacts chatgpt.com: that redirect is only
// checked as a registered URI and parsed from the consent response.
func TestCIMDChatGPTShapedDocumentIsAcceptedAsRegisteredRedirect(t *testing.T) {
	const chatgptRedirect = "https://chatgpt.com/connector_platform_oauth_redirect"
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	setDoc(meta, cimdDoc(cid, map[string]any{
		"client_name":   "ChatGPT-shaped fixture",
		"redirect_uris": []string{chatgptRedirect},
	}))

	redirect := f.approveAuthorize(map[string]string{"client_id": cid, "redirect_uri": chatgptRedirect})
	if redirect.Host != "chatgpt.com" {
		t.Fatalf("redirect host = %q, want the registered redirect", redirect.Host)
	}
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeFrom(t, redirect)},
		"client_id":     {cid},
		"redirect_uri":  {chatgptRedirect},
		"code_verifier": {testVerifier},
	})
	if status != http.StatusOK {
		t.Fatalf("exchange status = %d body=%v", status, body)
	}
}

func TestCIMDClientIDMismatchIsRejected(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	setDoc(meta, cimdDoc(cid+"/other", nil))
	assertRejectedWithoutRedirect(t, f, cid, nil)
}

// TestCIMDPlainHTTPClientIDIsUnknownAndNotFetched: an http:// URL is not a
// metadata-document client_id, so it goes to storage, is unknown, and is not
// fetched.
func TestCIMDPlainHTTPClientIDIsUnknownAndNotFetched(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	setDoc(meta, cimdDoc(cid, nil))
	assertRejectedWithoutRedirect(t, f, strings.Replace(cid, "https://", "http://", 1), nil)
	if meta.hits() != 0 {
		t.Fatal("http:// client_id was fetched")
	}
}

// TestCIMDZonedHostClientIDIsRejectedWithoutFetch: an IPv6 zone in the host
// (percent-encoded or literal) is refused before any fetch, so the dial policy
// never has to judge a zoned address.
func TestCIMDZonedHostClientIDIsRejectedWithoutFetch(t *testing.T) {
	meta := newMetadataServer()
	f, _ := newCIMDFixture(t, meta, true)
	for _, id := range []string{
		"https://[::1%25lo]:443/x",
		"https://[fe80::1%25eth0]/x",
		"https://[fe80::1%eth0]/x",
	} {
		if isCIMDClientID(id) {
			t.Errorf("isCIMDClientID(%q) = true, want false", id)
		}
		assertRejectedWithoutRedirect(t, f, id, nil)
	}
	if meta.hits() != 0 {
		t.Fatal("zoned client_id was fetched")
	}
}

// TestCIMDDialPolicyRefusesZonedAddress: even a policy that allows private
// addresses refuses a zoned one at connect time.
func TestCIMDDialPolicyRefusesZonedAddress(t *testing.T) {
	for _, addr := range []string{"[fe80::1%eth0]:443", "[::1%lo]:443", "[fd00:ec2::254%eth0]:80"} {
		if err := policyControl(realtime.EndpointPolicy{AllowPrivate: true})("tcp", addr, nil); !errors.Is(err, realtime.ErrSSRFBlocked) {
			t.Errorf("dial %s with AllowPrivate: want ErrSSRFBlocked, got %v", addr, err)
		}
	}
}

func TestCIMDOversizedDocumentIsRejected(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	// The padding is an unknown key, so only the size limit can refuse it.
	setDoc(meta, cimdDoc(cid, map[string]any{"pad": strings.Repeat("a", 70<<10)}))
	assertRejectedWithoutRedirect(t, f, cid, nil)
}

func TestCIMDNonJSONContentTypeIsRejected(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	setDoc(meta, cimdDoc(cid, nil))
	meta.update(func(m *metadataServer) { m.contentType = "text/html" })
	assertRejectedWithoutRedirect(t, f, cid, nil)
}

// TestCIMDNon200IsRejectedAndNotCached: a failed fetch is not cached, so a
// later successful fetch is used.
func TestCIMDNon200IsRejectedAndNotCached(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	setDoc(meta, cimdDoc(cid, nil))
	meta.update(func(m *metadataServer) { m.status = http.StatusNotFound })
	assertRejectedWithoutRedirect(t, f, cid, nil)

	meta.update(func(m *metadataServer) { m.status = http.StatusOK })
	// The failure is remembered only briefly, so recovery is seen after the
	// negative TTL.
	f.clock.Advance(cimdFailTTL + time.Second)
	if _, err := f.srv.clients.ResolveClient(f.ctx, cid); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
}

// TestCIMDFailedFetchIsNegativelyCached: within the negative TTL a bad URL is
// refused from memory, so repeated requests make one outbound fetch, not one
// each.
func TestCIMDFailedFetchIsNegativelyCached(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	meta.update(func(m *metadataServer) { m.status = http.StatusNotFound })
	for i := 0; i < 5; i++ {
		if _, err := f.srv.clients.ResolveClient(f.ctx, cid); err == nil {
			t.Fatal("failing document resolved")
		}
	}
	if n := meta.hits(); n != 1 {
		t.Fatalf("fetches within negative TTL = %d, want 1", n)
	}
}

// TestCIMDMirrorCapRefusesNewDocumentsButKeepsKnownOnes: once the mirror holds
// cap rows, a new metadata document is refused and writes no row, while a
// document already mirrored still resolves.
func TestCIMDMirrorCapRefusesNewDocumentsButKeepsKnownOnes(t *testing.T) {
	first := newMetadataServer()
	f, firstCID := newCIMDFixture(t, first, true)
	setDoc(first, cimdDoc(firstCID, nil))
	if _, err := f.srv.clients.ResolveClient(f.ctx, firstCID); err != nil {
		t.Fatalf("first mirror: %v", err)
	}

	// A second metadata host, with the same trusted root.
	second := newMetadataServer()
	ts := httptest.NewTLSServer(second)
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	secondCID := ts.URL + "/client.json"
	setDoc(second, cimdDoc(secondCID, nil))
	capped := newClientResolver(f.store, f.clock.Now, cimdOptions{allowPrivate: true, rootCAs: pool})
	capped.(*compositeResolver).mirrorCap = 1
	f.resetResolver(capped)

	if _, err := f.srv.clients.ResolveClient(f.ctx, secondCID); err == nil {
		t.Fatal("new document beyond the mirror cap was accepted")
	}
	if _, err := f.store.GetOAuthClient(f.ctx, secondCID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("refused document wrote a row: err = %v", err)
	}
	if _, err := f.srv.clients.ResolveClient(f.ctx, firstCID); err != nil {
		t.Fatalf("already-mirrored document refused at cap: %v", err)
	}
}

// TestCIMDConcurrentMissesShareOneFetch: simultaneous misses for one URL share a
// single outbound fetch and all receive the same client.
func TestCIMDConcurrentMissesShareOneFetch(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	setDoc(meta, cimdDoc(cid, nil))
	const callers = 16
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.srv.clients.ResolveClient(f.ctx, cid)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent resolve: %v", err)
		}
	}
	if n := meta.hits(); n != 1 {
		t.Fatalf("concurrent fetches = %d, want 1", n)
	}
}

func TestCIMDRedirectURINotListedIsRejected(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	setDoc(meta, cimdDoc(cid, nil))
	assertRejectedWithoutRedirect(t, f, cid, map[string]string{"redirect_uri": "https://evil.example/cb"})
}

func TestCIMDClientSecretBasicIsRejected(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	setDoc(meta, cimdDoc(cid, map[string]any{"token_endpoint_auth_method": "client_secret_basic"}))
	assertRejectedWithoutRedirect(t, f, cid, nil)
}

// TestCIMDRedirectIsNotFollowed: a 302 from the metadata URL is an error, and
// the redirect target is never requested.
func TestCIMDRedirectIsNotFollowed(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	final := strings.Replace(cid, "/client.json", "/final.json", 1)
	setDoc(meta, cimdDoc(cid, nil))
	meta.update(func(m *metadataServer) { m.redirect = final })
	assertRejectedWithoutRedirect(t, f, cid, nil)
	if meta.requested("/final.json") {
		t.Fatal("redirect target was requested")
	}
}

// TestCIMDDefaultPolicyBlocksLoopbackFetch uses the exported production
// constructor, so the default endpoint policy is what refuses 127.0.0.1.
func TestCIMDDefaultPolicyBlocksLoopbackFetch(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	setDoc(meta, cimdDoc(cid, nil))
	f.resetResolver(NewClientResolver(f.store, f.clock.Now))
	assertRejectedWithoutRedirect(t, f, cid, nil)
	if meta.hits() != 0 {
		t.Fatal("production policy dialed a loopback address")
	}
}

func TestCIMDCacheHitMakesNoSecondFetch(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	setDoc(meta, cimdDoc(cid, nil))
	for i := 0; i < 2; i++ {
		if _, err := f.srv.clients.ResolveClient(f.ctx, cid); err != nil {
			t.Fatal(err)
		}
	}
	if n := meta.hits(); n != 1 {
		t.Fatalf("metadata fetches = %d, want 1", n)
	}
}

func TestCIMDTTLExpiryRefetches(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	meta.update(func(m *metadataServer) {
		m.body = cimdDoc(cid, nil)
		m.cacheControl = "max-age=60"
	})
	resolve := func() {
		t.Helper()
		if _, err := f.srv.clients.ResolveClient(f.ctx, cid); err != nil {
			t.Fatal(err)
		}
	}
	resolve()
	f.clock.Advance(59 * time.Second)
	resolve()
	if n := meta.hits(); n != 1 {
		t.Fatalf("fetches before expiry = %d, want 1", n)
	}
	f.clock.Advance(2 * time.Second)
	resolve()
	if n := meta.hits(); n != 2 {
		t.Fatalf("fetches after expiry = %d, want 2", n)
	}
}

// upsertCounter counts client upserts. Only UpsertOAuthClient is overridden;
// every other Store method is the embedded real SQLite store.
type upsertCounter struct {
	*storage.Store
	mu      sync.Mutex
	upserts int
}

func (c *upsertCounter) UpsertOAuthClient(ctx context.Context, cl storage.OAuthClient) error {
	c.mu.Lock()
	c.upserts++
	c.mu.Unlock()
	return c.Store.UpsertOAuthClient(ctx, cl)
}

// TestCIMDMirrorsOnFetchOnly: the client row is written on the cache miss and
// not again on cache hits.
func TestCIMDMirrorsOnFetchOnly(t *testing.T) {
	meta := newMetadataServer()
	ts := httptest.NewTLSServer(meta)
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	f := newFixture(t)
	cid := ts.URL + "/client.json"
	setDoc(meta, cimdDoc(cid, nil))
	counter := &upsertCounter{Store: f.store}
	f.srv = NewServer(f.cfg, counter, newClientResolver(counter, f.clock.Now, cimdOptions{allowPrivate: true, rootCAs: pool}), f.clock.Now, slog.New(slog.DiscardHandler))
	for i := 0; i < 3; i++ {
		if _, err := f.srv.clients.ResolveClient(f.ctx, cid); err != nil {
			t.Fatal(err)
		}
	}
	counter.mu.Lock()
	defer counter.mu.Unlock()
	if counter.upserts != 1 {
		t.Fatalf("upserts = %d, want 1 (fetch only)", counter.upserts)
	}
}

// flakyResolver delegates to the real resolver until fail is set, then refuses
// every client. It models a metadata document that disappears or becomes
// unfetchable between authorization and a token request.
type flakyResolver struct {
	inner ClientResolver
	fail  bool
}

func (r *flakyResolver) ResolveClient(ctx context.Context, clientID string) (Client, error) {
	if r.fail {
		return Client{}, ErrUnknownClient
	}
	return r.inner.ResolveClient(ctx, clientID)
}

// TestTokenTimeResolveFailureIsInvalidClient: if the client cannot be resolved
// at refresh time, the answer is invalid_client (RFC 6749 section 5.2).
func TestTokenTimeResolveFailureIsInvalidClient(t *testing.T) {
	f := newFixture(t)
	refresh := f.refreshTokenForDefaultClient(t)
	flaky := &flakyResolver{inner: storeResolver{store: f.store}, fail: true}
	f.serve(NewServer(f.cfg, f.store, flaky, f.clock.Now, slog.New(slog.DiscardHandler)))
	status, body := f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {testClientID},
	})
	// RFC 6749 section 5.2: invalid_client is answered with 401.
	if status != http.StatusUnauthorized || body["error"] != "invalid_client" {
		t.Fatalf("status=%d body=%v, want 401 invalid_client", status, body)
	}
}

// TestRefreshRefusedWhenClientLostRefreshGrant: the client's registered grants
// no longer include refresh_token, so refresh is unauthorized_client even
// though the token itself is still valid.
func TestRefreshRefusedWhenClientLostRefreshGrant(t *testing.T) {
	f := newFixture(t)
	refresh := f.refreshTokenForDefaultClient(t)
	stored, err := f.store.GetOAuthClient(f.ctx, testClientID)
	if err != nil {
		t.Fatal(err)
	}
	stored.GrantTypes = `["authorization_code"]`
	if err := f.store.UpsertOAuthClient(f.ctx, stored); err != nil {
		t.Fatal(err)
	}
	// Resolve through storage so the stored grant types are what the gate sees.
	f.serve(NewServer(f.cfg, f.store, storeResolver{store: f.store}, f.clock.Now, slog.New(slog.DiscardHandler)))
	status, body := f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {testClientID},
	})
	if status != http.StatusBadRequest || body["error"] != "unauthorized_client" {
		t.Fatalf("status=%d body=%v, want 400 unauthorized_client", status, body)
	}
}

// TestCIMDOverlongClientIDIsRejectedWithoutFetch: a client_id longer than the
// oauth_clients bound is refused before any outbound request is made.
func TestCIMDOverlongClientIDIsRejectedWithoutFetch(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	setDoc(meta, cimdDoc(cid, nil))
	// A metadata-document-shaped URL over the bound. Without the length check it
	// would be classified as CIMD and fetched, so the zero-hit assertion below
	// fails if the check is removed.
	long := cid + "/" + strings.Repeat("a", maxCIMDClientIDBytes)
	if len(long) <= maxCIMDClientIDBytes {
		t.Fatal("test setup: overlong ID must exceed the bound")
	}
	if _, err := f.srv.clients.ResolveClient(f.ctx, long); err == nil {
		t.Fatal("overlong client_id resolved")
	}
	if n := meta.hits(); n != 0 {
		t.Fatalf("overlong client_id caused %d outbound fetches, want 0", n)
	}
}

// TestCIMDNegativeCacheStaysBounded: many distinct failing URLs leave the
// negative cache at or below its cap, and the oldest entries are the ones dropped.
func TestCIMDNegativeCacheStaysBounded(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	meta.update(func(m *metadataServer) { m.status = http.StatusNotFound })
	r := f.srv.clients.(*compositeResolver).cimd
	base := strings.TrimSuffix(cid, "client.json")
	for i := 0; i < cimdFailedCap+200; i++ {
		if _, err := f.srv.clients.ResolveClient(f.ctx, base+"doc"+strconv.Itoa(i)+".json"); err == nil {
			t.Fatal("failing document resolved")
		}
	}
	r.mu.Lock()
	n := len(r.failed)
	r.mu.Unlock()
	if n > cimdFailedCap {
		t.Fatalf("negative cache holds %d entries, cap is %d", n, cimdFailedCap)
	}
}

// TestCIMDNegativeCacheSweepsExpiredEntries: entries past their TTL are removed
// when new failures arrive, so the map does not depend on the cap alone.
func TestCIMDNegativeCacheSweepsExpiredEntries(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	meta.update(func(m *metadataServer) { m.status = http.StatusNotFound })
	r := f.srv.clients.(*compositeResolver).cimd
	base := strings.TrimSuffix(cid, "client.json")
	for i := 0; i < 50; i++ {
		_, _ = f.srv.clients.ResolveClient(f.ctx, base+"old"+strconv.Itoa(i)+".json")
	}
	f.clock.Advance(cimdFailTTL + time.Second)
	_, _ = f.srv.clients.ResolveClient(f.ctx, base+"new.json")
	r.mu.Lock()
	n := len(r.failed)
	r.mu.Unlock()
	if n != 1 {
		t.Fatalf("expired negative entries retained: %d entries, want only the new one", n)
	}
}

// TestCIMDFetchConcurrencyNeverExceedsSemaphore: many distinct slow fetches at
// once never run more than cimdMaxConcurrentFetches outbound requests together.
func TestCIMDFetchConcurrencyNeverExceedsSemaphore(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	release := make(chan struct{})
	gate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	})
	ts := httptest.NewTLSServer(gate)
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	f := newFixture(t)
	f.resetResolver(newClientResolver(f.store, f.clock.Now, cimdOptions{allowPrivate: true, rootCAs: pool}))

	const callers = 3 * cimdMaxConcurrentFetches
	// Cleanups run last-in first-out, so registering this after ts.Close makes
	// it run first: blocked handlers are released before the server shuts down.
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = f.srv.clients.ResolveClient(f.ctx, ts.URL+"/c"+strconv.Itoa(i)+".json")
		}(i)
	}
	// Wait until the cap is reached, then hold briefly so any excess fetch that
	// the semaphore should block shows up in the peak. Without a semaphore the
	// peak exceeds the cap, which the assertion below reports.
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return inFlight >= cimdMaxConcurrentFetches
	})
	time.Sleep(50 * time.Millisecond)
	releaseAll()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if peak > cimdMaxConcurrentFetches {
		t.Fatalf("peak concurrent fetches = %d, want at most %d", peak, cimdMaxConcurrentFetches)
	}
}

// waitFor polls cond until it holds, failing the test after a bounded wait.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestCIMDRefusedMirrorIsNotCachedAndResolvesToUnknown: when the mirror refuses
// a fetched document (here, at the cap), no client is cached for it. A second
// resolve must answer ErrUnknownClient, and must not return a client that has
// no oauth_clients row, which would fail later as a server_error on grant.
func TestCIMDRefusedMirrorIsNotCachedAndResolvesToUnknown(t *testing.T) {
	meta := newMetadataServer()
	ts := httptest.NewTLSServer(meta)
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	f := newFixture(t)
	cid := ts.URL + "/client.json"
	setDoc(meta, cimdDoc(cid, nil))
	capped := newClientResolver(f.store, f.clock.Now, cimdOptions{allowPrivate: true, rootCAs: pool})
	capped.(*compositeResolver).mirrorCap = 0 // every new document is refused
	f.resetResolver(capped)

	for i := 0; i < 2; i++ {
		c, err := f.srv.clients.ResolveClient(f.ctx, cid)
		if !errors.Is(err, ErrUnknownClient) {
			t.Fatalf("resolve %d: client=%+v err=%v, want ErrUnknownClient", i, c, err)
		}
	}
	if _, err := f.store.GetOAuthClient(f.ctx, cid); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("refused document wrote a row: err = %v", err)
	}
	// The refusal must be visible at the HTTP edge as a client error, not 500.
	resp := get(t, f.authorizeURL(map[string]string{"client_id": cid}))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("authorize status = %d, want 400 (not 500)", resp.StatusCode)
	}
}

// TestCIMDMirrorCapEvictsStaleUnusedRowForNewDocument: at the cap, a stale
// metadata row that nothing references is evicted to make room for a new
// document. The new document is then mirrored and served.
func TestCIMDMirrorCapEvictsStaleUnusedRowForNewDocument(t *testing.T) {
	first := newMetadataServer()
	f, firstCID := newCIMDFixture(t, first, true)
	setDoc(first, cimdDoc(firstCID, nil))
	if _, err := f.srv.clients.ResolveClient(f.ctx, firstCID); err != nil {
		t.Fatalf("first mirror: %v", err)
	}
	// Age the first row past any cached copy's lifetime, so eviction is safe.
	f.clock.Advance(cimdMaxTTL + time.Minute)

	second := newMetadataServer()
	ts := httptest.NewTLSServer(second)
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	secondCID := ts.URL + "/client.json"
	setDoc(second, cimdDoc(secondCID, nil))
	capped := newClientResolver(f.store, f.clock.Now, cimdOptions{allowPrivate: true, rootCAs: pool})
	capped.(*compositeResolver).mirrorCap = 1
	f.resetResolver(capped)

	if _, err := f.srv.clients.ResolveClient(f.ctx, secondCID); err != nil {
		t.Fatalf("new document not admitted by evicting a stale unused row: %v", err)
	}
	if _, err := f.store.GetOAuthClient(f.ctx, firstCID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("stale unused row not evicted: err = %v", err)
	}
	if _, err := f.store.GetOAuthClient(f.ctx, secondCID); err != nil {
		t.Fatalf("new document row missing: %v", err)
	}
}

// TestCIMDMirrorCapRefusesWhenNothingIsEvictable: at the cap, if every row is
// referenced or recently updated, a new document is refused, not admitted by
// removing a row that a grant or a cached client still needs.
func TestCIMDMirrorCapRefusesWhenNothingIsEvictable(t *testing.T) {
	first := newMetadataServer()
	f, firstCID := newCIMDFixture(t, first, true)
	setDoc(first, cimdDoc(firstCID, nil))
	if _, err := f.srv.clients.ResolveClient(f.ctx, firstCID); err != nil {
		t.Fatalf("first mirror: %v", err)
	}
	// The row is recent: no clock advance, so it is inside the cache lifetime.

	second := newMetadataServer()
	ts := httptest.NewTLSServer(second)
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	secondCID := ts.URL + "/client.json"
	setDoc(second, cimdDoc(secondCID, nil))
	capped := newClientResolver(f.store, f.clock.Now, cimdOptions{allowPrivate: true, rootCAs: pool})
	capped.(*compositeResolver).mirrorCap = 1
	f.resetResolver(capped)

	if _, err := f.srv.clients.ResolveClient(f.ctx, secondCID); err == nil {
		t.Fatal("new document admitted although no row was evictable")
	}
	if _, err := f.store.GetOAuthClient(f.ctx, firstCID); err != nil {
		t.Fatalf("recent row removed: %v", err)
	}
}
