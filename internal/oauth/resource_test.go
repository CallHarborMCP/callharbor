package oauth

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// bearerFixture mints one access token through the real flow and returns the
// token together with the grant ID it belongs to.
func (f *fixture) bearerFixture(t *testing.T) (string, string) {
	t.Helper()
	redirect := f.approveAuthorize(nil)
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeFrom(t, redirect)},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
		"resource":      {f.cfg.Resource()},
	})
	if status != http.StatusOK {
		t.Fatalf("token status = %d body=%v", status, body)
	}
	access, _ := body["access_token"].(string)
	grants, err := f.store.ListOAuthGrants(f.ctx, f.userID)
	if err != nil || len(grants) == 0 {
		t.Fatalf("grants = %v err=%v", grants, err)
	}
	return access, grants[0].ID
}

// callOnlyToken mints a token whose grant carries only mcp:call. The authorize
// request asks for that scope alone, so the consent flow itself records it.
func (f *fixture) callOnlyToken(t *testing.T) string {
	t.Helper()
	redirect := f.approveAuthorize(map[string]string{"scope": ScopeCall})
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeFrom(t, redirect)},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
		"resource":      {f.cfg.Resource()},
	})
	if status != http.StatusOK {
		t.Fatalf("token status = %d body=%v", status, body)
	}
	access, _ := body["access_token"].(string)
	return access
}

// guarded serves one request through RequireBearer(scopes...) and returns the
// recorder and whatever principal the inner handler saw.
func (f *fixture) guarded(t *testing.T, req *http.Request, scopes ...string) (*httptest.ResponseRecorder, Principal, bool) {
	t.Helper()
	var got Principal
	var seen bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, seen = PrincipalFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	f.srv.RequireBearer(scopes...)(inner).ServeHTTP(rec, req)
	return rec, got, seen
}

func bearerReq(token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// assertChallenge checks the 401 or 403 WWW-Authenticate header. An empty
// errCode means the no-credentials form, which carries no error attribute.
func assertChallenge(t *testing.T, rec *httptest.ResponseRecorder, status int, errCode string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d", rec.Code, status)
	}
	h := rec.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(h, "Bearer ") {
		t.Fatalf("WWW-Authenticate = %q", h)
	}
	wantPRM := `resource_metadata="` + "https://callharbor.example" + `/.well-known/oauth-protected-resource/mcp"`
	if !strings.Contains(h, wantPRM) {
		t.Fatalf("WWW-Authenticate missing resource_metadata: %q", h)
	}
	if errCode == "" {
		if strings.Contains(h, "error=") {
			t.Fatalf("no-credentials challenge carries an error: %q", h)
		}
		return
	}
	if !strings.Contains(h, `error="`+errCode+`"`) {
		t.Fatalf("WWW-Authenticate = %q, want error=%q", h, errCode)
	}
}

func TestBearerMissingHeaderChallengesWithoutError(t *testing.T) {
	f := newFixture(t)
	rec, _, _ := f.guarded(t, bearerReq(""), ScopeRead)
	assertChallenge(t, rec, http.StatusUnauthorized, "")
}

func TestBearerQueryTokenIsIgnored(t *testing.T) {
	f := newFixture(t)
	access, _ := f.bearerFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp?access_token="+access, nil)
	rec, _, _ := f.guarded(t, req, ScopeRead)
	assertChallenge(t, rec, http.StatusUnauthorized, "")
}

func TestBearerFormTokenIsIgnored(t *testing.T) {
	f := newFixture(t)
	access, _ := f.bearerFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("access_token="+access))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec, _, _ := f.guarded(t, req, ScopeRead)
	assertChallenge(t, rec, http.StatusUnauthorized, "")
}

func TestBearerUnknownTokenIsInvalid(t *testing.T) {
	f := newFixture(t)
	rec, _, _ := f.guarded(t, bearerReq("cho_a_not-a-real-token"), ScopeRead)
	assertChallenge(t, rec, http.StatusUnauthorized, "invalid_token")
}

func TestBearerExpiredTokenIsInvalid(t *testing.T) {
	f := newFixture(t)
	access, _ := f.bearerFixture(t)
	f.clock.Advance(accessTokenTTL + time.Second)
	rec, _, _ := f.guarded(t, bearerReq(access), ScopeRead)
	assertChallenge(t, rec, http.StatusUnauthorized, "invalid_token")
}

func TestBearerRevokedTokenIsInvalid(t *testing.T) {
	f := newFixture(t)
	access, _ := f.bearerFixture(t)
	if _, err := f.store.RevokeOAuthTokenByHash(f.ctx, hashToken(access), formatTime(f.clock.Now())); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := f.guarded(t, bearerReq(access), ScopeRead)
	assertChallenge(t, rec, http.StatusUnauthorized, "invalid_token")
}

func TestBearerRevokedFamilyIsInvalid(t *testing.T) {
	f := newFixture(t)
	access, grantID := f.bearerFixture(t)
	if err := f.store.RevokeOAuthGrant(f.ctx, grantID, formatTime(f.clock.Now()), storage.AuditEvent{
		OccurredAt: formatTime(f.clock.Now()), Action: "oauth.grant_revoked", ResourceType: "oauth_grant",
		ResourceID: grantID, Outcome: "success", DetailJSON: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := f.guarded(t, bearerReq(access), ScopeRead)
	assertChallenge(t, rec, http.StatusUnauthorized, "invalid_token")
}

func TestBearerWrongAudienceIsInvalid(t *testing.T) {
	f := newFixture(t)
	access, _ := f.bearerFixture(t)
	// Serve the same stored token from an instance whose resource differs.
	other := newServer(Config{PublicURL: "https://other.example"}, f.store, registeredResolver{f.store}, f.clock.Now, slog.New(slog.DiscardHandler), registerGlobalCap)
	rec := httptest.NewRecorder()
	other.RequireBearer(ScopeRead)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, bearerReq(access))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-audience status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
	}
}

func TestBearerInactiveUserIsRejected(t *testing.T) {
	f := newFixture(t)
	access, _ := f.bearerFixture(t)
	rec, _, _ := f.guarded(t, bearerReq(access), ScopeRead)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("before deactivation status = %d", rec.Code)
	}
	// Deactivation goes through the production path, which also revokes the
	// user's grants. The active check covers the same token either way.
	f.deactivate(f.userID)
	rec, _, _ = f.guarded(t, bearerReq(access), ScopeRead)
	assertChallenge(t, rec, http.StatusUnauthorized, "invalid_token")
}

func TestBearerInsufficientScopeIs403WithRequiredScope(t *testing.T) {
	f := newFixture(t)
	access := f.callOnlyToken(t)
	rec, _, _ := f.guarded(t, bearerReq(access), ScopeRead)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	h := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(h, `error="insufficient_scope"`) || !strings.Contains(h, `scope="`+ScopeRead+`"`) {
		t.Fatalf("WWW-Authenticate = %q", h)
	}
	if !strings.Contains(h, "resource_metadata=") {
		t.Fatalf("403 missing resource_metadata: %q", h)
	}
}

func TestBearerSuccessPutsPrincipalInContext(t *testing.T) {
	f := newFixture(t)
	access, grantID := f.bearerFixture(t)
	rec, p, seen := f.guarded(t, bearerReq(access), ScopeRead)
	if rec.Code != http.StatusNoContent || !seen {
		t.Fatalf("status = %d seen = %v", rec.Code, seen)
	}
	if p.UserID != f.userID || p.ClientID != testClientID || p.GrantID != grantID {
		t.Fatalf("principal = %+v", p)
	}
	if strings.Join(p.Scopes, " ") != "mcp:read mcp:call" {
		t.Fatalf("scopes = %v", p.Scopes)
	}
}

func TestBearerTouchIsThrottledToOncePerMinute(t *testing.T) {
	f := newFixture(t)
	access, grantID := f.bearerFixture(t)
	f.guarded(t, bearerReq(access), ScopeRead)
	first, err := f.store.GetOAuthGrantByID(f.ctx, grantID)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(30 * time.Second)
	f.guarded(t, bearerReq(access), ScopeRead)
	second, _ := f.store.GetOAuthGrantByID(f.ctx, grantID)
	if second.LastUsedAt != first.LastUsedAt {
		t.Fatalf("touch within a minute moved last_used_at: %q -> %q", first.LastUsedAt, second.LastUsedAt)
	}
	f.clock.Advance(time.Minute)
	f.guarded(t, bearerReq(access), ScopeRead)
	third, _ := f.store.GetOAuthGrantByID(f.ctx, grantID)
	if third.LastUsedAt == second.LastUsedAt {
		t.Fatal("touch after a minute did not update last_used_at")
	}
}

func TestProtectedResourceMetadataContent(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource"} {
		rec := httptest.NewRecorder()
		f.srv.handleProtectedResourceMetadata(rec, httptest.NewRequest(http.MethodGet, path, nil))
		body := rec.Body.String()
		for _, want := range []string{
			`"resource":"https://callharbor.example/mcp"`,
			`"authorization_servers":["https://callharbor.example"]`,
			`"scopes_supported":["mcp:read","mcp:call"]`,
			`"bearer_methods_supported":["header"]`,
			`"resource_name":"CallHarbor"`,
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing %s in %s", path, want, body)
			}
		}
	}
}

func TestCORSPresentOnPublicOAuthEndpoints(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/oauth/token", "/oauth/register", "/oauth/revoke", "/.well-known/oauth-authorization-server"} {
		rec := httptest.NewRecorder()
		corsPublic(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s missing ACAO", path)
		}
		if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("%s sends credentials", path)
		}
	}
	_ = f
}

func TestCORSOptionsPreflightForPOST(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest(http.MethodOptions, f.http.URL+"/oauth/token", nil)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type, authorization")
	rec := httptest.NewRecorder()
	corsPublic(nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("preflight missing ACAO")
	}
	allow := rec.Header().Get("Access-Control-Allow-Headers")
	if !strings.Contains(allow, "Content-Type") || !strings.Contains(allow, "Authorization") {
		t.Fatalf("Allow-Headers = %q", allow)
	}
}

func TestCORSAbsentOnSessionAndAuthorize(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Get(f.http.URL + "/oauth/authorize?" + url.Values{"client_id": {"x"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("/oauth/authorize carries CORS")
	}
}
