package app

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"testing"

	"golang.org/x/oauth2"
)

const (
	e2eMCPPath      = "/mcp"
	e2eCallbackPath = "/callback"
	e2eState        = "st-e2e"
)

var resourceMetadataRE = regexp.MustCompile(`resource_metadata="([^"]+)"`)

// e2eEnv is one running instance. admin is nil for an instance that another
// test set up and attached to.
type e2eEnv struct {
	t      *testing.T
	base   string
	client *http.Client
	admin  *session
}

// startE2E runs the app on a fresh listener whose origin is PublicURL, then
// creates the first administrator.
func startE2E(t *testing.T, dataDir string) *e2eEnv {
	t.Helper()
	ln := listen(t)
	base := "http://" + ln.Addr().String()
	startInstance(t, dataDir, ln, base)
	c := newE2EClient()
	return &e2eEnv{t: t, base: base, client: c, admin: setupAdmin(t, c, base)}
}

func (e *e2eEnv) resource() string { return e.base + e2eMCPPath }

// callbackURL returns a loopback redirect target. Nothing listens on it; the
// server only compares it.
func callbackURL(t *testing.T) string {
	t.Helper()
	return "http://" + listen(t).Addr().String() + e2eCallbackPath
}

// jsonHas reports whether a decoded JSON array contains want.
func jsonHas(v any, want string) bool {
	list, _ := v.([]any)
	for _, x := range list {
		if x == want {
			return true
		}
	}
	return false
}

// register performs dynamic client registration for a public PKCE client.
func (e *e2eEnv) register(redirect string) string {
	e.t.Helper()
	status, _, body := sendJSON(e.t, e.client, http.MethodPost, e.base+"/oauth/register", map[string]any{
		"client_name":                "e2e",
		"redirect_uris":              []string{redirect},
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	}, nil)
	if status != http.StatusCreated {
		e.t.Fatalf("register = %d %s", status, body)
	}
	id, _ := decodeObject(e.t, body)["client_id"].(string)
	if id == "" {
		e.t.Fatal("register returned no client_id")
	}
	return id
}

func (e *e2eEnv) oauthConfig(clientID, redirect string, scopes ...string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:    clientID,
		RedirectURL: redirect,
		Scopes:      scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:   e.base + "/oauth/authorize",
			TokenURL:  e.base + "/oauth/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
}

// startAuthorize sends the browser to /oauth/authorize and returns the consent
// request ID from the 302 to /oauth/consent.
func (e *e2eEnv) startAuthorize(conf *oauth2.Config, verifier string) string {
	e.t.Helper()
	target := conf.AuthCodeURL(e2eState,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("resource", e.resource()))
	status, hdr, body := send(e.t, e.client, http.MethodGet, target, nil, nil)
	if status != http.StatusFound {
		e.t.Fatalf("authorize = %d %s", status, body)
	}
	loc, err := url.Parse(hdr.Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}
	if loc.Path != "/oauth/consent" {
		e.t.Fatalf("authorize redirected to %s, want /oauth/consent", loc.String())
	}
	id := loc.Query().Get("request")
	if id == "" {
		e.t.Fatal("consent redirect carries no request id")
	}
	return id
}

// consentView fetches the consent screen data as the signed-in user.
func (e *e2eEnv) consentView(user *session, requestID string) map[string]any {
	e.t.Helper()
	status, _, body := send(e.t, user.client, http.MethodGet,
		e.base+"/api/oauth/requests/"+requestID, nil, user.headers(false))
	if status != http.StatusOK {
		e.t.Fatalf("consent view = %d %s", status, body)
	}
	return decodeObject(e.t, body)
}

// decide records the user's answer and returns the redirect the client follows.
func (e *e2eEnv) decide(user *session, requestID string, approve bool) *url.URL {
	e.t.Helper()
	status, _, body := sendJSON(e.t, user.client, http.MethodPost,
		e.base+"/api/oauth/requests/"+requestID+"/decision",
		map[string]bool{"approve": approve}, user.headers(true))
	if status != http.StatusOK {
		e.t.Fatalf("decision = %d %s", status, body)
	}
	raw, _ := decodeObject(e.t, body)["redirect_url"].(string)
	u, err := url.Parse(raw)
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

// exchange checks the redirect's state, iss and code, then redeems the code
// through x/oauth2 with the PKCE verifier and resource.
func (e *e2eEnv) exchange(conf *oauth2.Config, verifier string, back *url.URL) *oauth2.Token {
	e.t.Helper()
	if got := back.Scheme + "://" + back.Host + back.Path; got != conf.RedirectURL {
		e.t.Fatalf("redirect target = %s, want %s", got, conf.RedirectURL)
	}
	q := back.Query()
	if q.Get("state") != e2eState {
		e.t.Fatalf("state = %q", q.Get("state"))
	}
	if q.Get("iss") != e.base {
		e.t.Fatalf("iss = %q, want %q", q.Get("iss"), e.base)
	}
	code := q.Get("code")
	if code == "" {
		e.t.Fatal("redirect carries no code")
	}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, e.client)
	tok, err := conf.Exchange(ctx, code,
		oauth2.VerifierOption(verifier),
		oauth2.SetAuthURLParam("resource", e.resource()))
	if err != nil {
		e.t.Fatalf("exchange: %v", err)
	}
	return tok
}

// grant runs authorize, consent approval and code exchange for user.
func (e *e2eEnv) grant(user *session, clientID, redirect string, scopes ...string) *oauth2.Token {
	e.t.Helper()
	verifier := oauth2.GenerateVerifier()
	conf := e.oauthConfig(clientID, redirect, scopes...)
	reqID := e.startAuthorize(conf, verifier)
	e.consentView(user, reqID)
	return e.exchange(conf, verifier, e.decide(user, reqID, true))
}

// postForm posts an x-www-form-urlencoded body and decodes the JSON reply.
func (e *e2eEnv) postForm(target string, form url.Values) (int, http.Header, map[string]any) {
	e.t.Helper()
	h := http.Header{}
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	status, hdr, body := send(e.t, e.client, http.MethodPost, target, []byte(form.Encode()), h)
	return status, hdr, decodeObject(e.t, body)
}

// mcpWith sends one request to /mcp with a bearer token.
func (e *e2eEnv) mcpWith(token string) (int, http.Header, []byte) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	return sendJSON(e.t, e.client, http.MethodPost, e.base+e2eMCPPath,
		map[string]string{"jsonrpc": "2.0", "id": "1", "method": "ping"}, h)
}

// grantFor returns the caller's live grant ID for clientID, or "".
func (e *e2eEnv) grantFor(user *session, clientID string) string {
	e.t.Helper()
	status, _, body := send(e.t, user.client, http.MethodGet,
		e.base+"/api/oauth/grants", nil, user.headers(false))
	if status != http.StatusOK {
		e.t.Fatalf("grants = %d %s", status, body)
	}
	list, _ := decodeObject(e.t, body)["grants"].([]any)
	for _, g := range list {
		m, _ := g.(map[string]any)
		if m["client_id"] == clientID {
			id, _ := m["id"].(string)
			return id
		}
	}
	return ""
}

// userID returns the signed-in user's ID from GET /api/session.
func (e *e2eEnv) userID(user *session) string {
	e.t.Helper()
	status, _, body := send(e.t, user.client, http.MethodGet,
		e.base+"/api/session", nil, user.headers(false))
	if status != http.StatusOK {
		e.t.Fatalf("session = %d %s", status, body)
	}
	u, _ := decodeObject(e.t, body)["user"].(map[string]any)
	id, _ := u["id"].(string)
	if id == "" {
		e.t.Fatal("session returned no user id")
	}
	return id
}
