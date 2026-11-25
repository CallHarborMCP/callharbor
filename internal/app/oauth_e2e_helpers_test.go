package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/observability"
)

const e2ePassword = "correct-horse-battery"

// newE2EClient never follows redirects, so tests can inspect each 302. It uses
// a private transport without keep-alive: a pooled or speculative connection
// left in StateNew would block the instance's graceful shutdown.
func newE2EClient() *http.Client {
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// startInstance runs the real app on ln with the given public origin and waits
// until it is ready. Shutdown is registered with t.Cleanup.
func startInstance(t *testing.T, dataDir string, ln net.Listener, publicURL string) {
	t.Helper()
	startInstanceWithProxies(t, dataDir, ln, publicURL, nil)
}

// startInstanceWithProxies is startInstance with CALLHARBOR_TRUSTED_PROXIES
// already parsed into trusted, so a test can prove that the list reaches the
// OAuth server through Run.
func startInstanceWithProxies(t *testing.T, dataDir string, ln net.Listener, publicURL string, trusted []netip.Prefix) {
	t.Helper()
	cfg := testConfig(t)
	cfg.DataDir = dataDir
	cfg.PublicURL = publicURL
	cfg.TrustedProxies = trusted
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, observability.NewLogger(io.Discard, slog.LevelError), ln)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run() = %v, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Run() did not return after cancellation")
		}
	})
	waitForStatus(t, newE2EClient(), "http://"+ln.Addr().String()+"/readyz", http.StatusOK)
}

// send performs one request and returns status, headers and the full body.
func send(t *testing.T, c *http.Client, method, target string, body []byte, h http.Header) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, b
}

// sendJSON marshals v as the JSON body of one request.
func sendJSON(t *testing.T, c *http.Client, method, target string, v any, h http.Header) (int, http.Header, []byte) {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if h == nil {
		h = http.Header{}
	}
	h.Set("Content-Type", "application/json")
	return send(t, c, method, target, body, h)
}

// decodeObject parses a JSON object body.
func decodeObject(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return out
}

// session is a signed-in browser-equivalent. The session cookie is marked
// Secure, so a cookie jar would not send it over plain HTTP; the cookie is
// therefore attached by hand.
type session struct {
	base   string
	client *http.Client
	token  string
	csrf   string
}

// login signs in and keeps the session cookie and CSRF token.
func login(t *testing.T, client *http.Client, base, username string) *session {
	t.Helper()
	status, hdr, body := sendJSON(t, client, http.MethodPost, base+"/api/session",
		map[string]string{"username": username, "password": e2ePassword}, nil)
	if status != http.StatusOK {
		t.Fatalf("login %s = %d %s", username, status, body)
	}
	s := &session{base: base, client: client, csrf: decodeObject(t, body)["csrf_token"].(string)}
	for _, c := range (&http.Response{Header: hdr}).Cookies() {
		if c.Name == identity.SessionCookieName {
			s.token = c.Value
		}
	}
	if s.token == "" {
		t.Fatalf("login %s set no session cookie", username)
	}
	return s
}

// headers returns the cookie, and when csrf is true the CSRF header too.
func (s *session) headers(csrf bool) http.Header {
	h := http.Header{}
	h.Set("Cookie", identity.SessionCookieName+"="+s.token)
	if csrf {
		h.Set("X-CSRF-Token", s.csrf)
	}
	return h
}

// setupAdmin creates the first administrator through the public setup API.
func setupAdmin(t *testing.T, client *http.Client, base string) *session {
	t.Helper()
	status, _, body := sendJSON(t, client, http.MethodPost, base+"/api/setup",
		map[string]string{"username": "admin", "password": e2ePassword}, nil)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("setup = %d %s", status, body)
	}
	return login(t, client, base, "admin")
}

// createUser adds a normal user through the admin API and returns their session.
func createUser(t *testing.T, admin *session, username string) *session {
	t.Helper()
	status, _, body := sendJSON(t, admin.client, http.MethodPost, admin.base+"/api/users",
		map[string]string{"username": username, "password": e2ePassword, "role": "user"}, admin.headers(true))
	if status != http.StatusCreated {
		t.Fatalf("create user %s = %d %s", username, status, body)
	}
	return login(t, admin.client, admin.base, username)
}
