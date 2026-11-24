package oauth

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

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
	f.serve(NewServer(f.cfg, f.store, clients, f.clock.Now))
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
	if _, err := f.srv.clients.ResolveClient(f.ctx, cid); err != nil {
		t.Fatalf("after recovery: %v", err)
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
	f.srv = NewServer(f.cfg, counter, newClientResolver(counter, f.clock.Now, cimdOptions{allowPrivate: true, rootCAs: pool}), f.clock.Now)
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
	f.serve(NewServer(f.cfg, f.store, flaky, f.clock.Now))
	status, body := f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {testClientID},
	})
	// No Authorization header was sent, so RFC 6749 section 5.2 does not
	// require 401; invalid_client is returned with 400.
	if status != http.StatusBadRequest || body["error"] != "invalid_client" {
		t.Fatalf("status=%d body=%v, want 400 invalid_client", status, body)
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
	f.serve(NewServer(f.cfg, f.store, storeResolver{store: f.store}, f.clock.Now))
	status, body := f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {testClientID},
	})
	if status != http.StatusBadRequest || body["error"] != "unauthorized_client" {
		t.Fatalf("status=%d body=%v, want 400 unauthorized_client", status, body)
	}
}
