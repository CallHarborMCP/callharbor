package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

const (
	testClientID    = "https://client.example/meta.json"
	testRedirectURI = "https://client.example/cb"
	testVerifier    = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	// testChallenge is the S256 challenge of testVerifier (RFC 7636 appendix B).
	testChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
)

// fixedClock is a deterministic clock the tests advance explicitly.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// registeredResolver resolves only clients present in storage, which is the
// chunk A behavior. CIMD resolution arrives in chunk B.
type registeredResolver struct{ store *storage.Store }

func (r registeredResolver) ResolveClient(ctx context.Context, clientID string) (Client, error) {
	c, err := r.store.GetOAuthClient(ctx, clientID)
	if err != nil {
		return Client{}, ErrUnknownClient
	}
	var uris []string
	if err := json.Unmarshal([]byte(c.RedirectURIs), &uris); err != nil {
		return Client{}, fmt.Errorf("oauth: stored redirect_uris: %w", err)
	}
	return Client{ID: c.ClientID, Name: c.ClientName, URI: c.ClientURI, RedirectURIs: uris}, nil
}

// fixture is a real SQLite store with a user, a registered client, and an
// authorization server wired to them.
type fixture struct {
	t      *testing.T
	ctx    context.Context
	store  *storage.Store
	clock  *fixedClock
	srv    *Server
	http   *httptest.Server
	userID string
	cfg    Config
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := storage.Open(ctx, filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	clk := &fixedClock{now: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)}
	ident, err := identity.NewService(st, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	u, err := ident.Setup(ctx, "alice", "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	// A second active admin keeps the last-admin guard from blocking the
	// deactivation tests, which are about OAuth state, not admin policy.
	if _, err := ident.CreateUser(ctx, &identity.Principal{User: u}, "bob", "correct-horse-battery", identity.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, ctx: ctx, store: st, clock: clk, userID: u.ID}
	f.cfg = Config{PublicURL: "https://callharbor.example"}
	f.srv = NewServer(f.cfg, st, registeredResolver{st}, clk.Now)
	f.registerClient(testClientID, []string{testRedirectURI})

	// Consent decisions are exercised through the test session principal:
	// every decision is made by the fixture's signed-in user.
	mux := http.NewServeMux()
	f.srv.Register(mux)
	mux.Handle("POST /oauth/consent/decision", f.srv.ServeDecision(func(*http.Request) (string, bool) {
		return f.userID, true
	}))
	f.http = httptest.NewServer(mux)
	t.Cleanup(f.http.Close)
	return f
}

// registerClient stores a public client with the given redirect URIs.
func (f *fixture) registerClient(id string, redirects []string) {
	f.t.Helper()
	raw, err := json.Marshal(redirects)
	if err != nil {
		f.t.Fatal(err)
	}
	now := formatTime(f.clock.Now())
	if err := f.store.UpsertOAuthClient(f.ctx, storage.OAuthClient{
		ClientID:     id,
		Source:       "metadata_document",
		ClientName:   "Test Client",
		RedirectURIs: string(raw),
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		f.t.Fatal(err)
	}
}

// authorizeURL builds an authorize request. Values in params override the
// defaults; an empty value omits the parameter entirely.
func (f *fixture) authorizeURL(params map[string]string) string {
	base := map[string]string{
		"response_type":         "code",
		"client_id":             testClientID,
		"redirect_uri":          testRedirectURI,
		"scope":                 "mcp:read mcp:call",
		"state":                 "xyz",
		"code_challenge":        testChallenge,
		"code_challenge_method": "S256",
		"resource":              f.cfg.Resource(),
	}
	for k, v := range params {
		if v == "" {
			delete(base, k)
		} else {
			base[k] = v
		}
	}
	q := url.Values{}
	for k, v := range base {
		q.Set(k, v)
	}
	return f.http.URL + "/oauth/authorize?" + q.Encode()
}

// noRedirectClient returns an HTTP client that does not follow redirects, so
// tests can inspect the Location header.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// Compile-time check that the real store satisfies the handler's interface.
var _ Store = (*storage.Store)(nil)
