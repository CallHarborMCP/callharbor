package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

func TestLoadConsentRequestShowsPendingRequestOnly(t *testing.T) {
	f := newFixture(t)
	id := location(t, get(t, f.authorizeURL(nil))).Query().Get("request")

	got, err := f.srv.LoadConsentRequest(f.ctx, id)
	if err != nil {
		t.Fatalf("pending request: %v", err)
	}
	if got.ClientID != testClientID || got.Resource != f.cfg.Resource() {
		t.Fatalf("consent view = %+v", got)
	}
	if strings.Join(got.Scopes, " ") != "mcp:read mcp:call" {
		t.Fatalf("scopes = %v", got.Scopes)
	}

	f.consentFor(id, true)
	if _, err := f.srv.LoadConsentRequest(f.ctx, id); err != ErrRequestNotFound {
		t.Fatalf("decided request err = %v, want ErrRequestNotFound", err)
	}
}

func TestLoadConsentRequestUnknownAndExpired(t *testing.T) {
	f := newFixture(t)
	if _, err := f.srv.LoadConsentRequest(f.ctx, "cho_q_missing"); err != ErrRequestNotFound {
		t.Fatalf("unknown err = %v", err)
	}
	id := location(t, get(t, f.authorizeURL(nil))).Query().Get("request")
	f.clock.Advance(authRequestTTL + time.Second)
	if _, err := f.srv.LoadConsentRequest(f.ctx, id); err != ErrRequestNotFound {
		t.Fatalf("expired err = %v", err)
	}
}

func TestDenyAfterDecisionIsNotFound(t *testing.T) {
	f := newFixture(t)
	id := location(t, get(t, f.authorizeURL(nil))).Query().Get("request")
	f.consentFor(id, true)
	if _, err := f.srv.Decide(f.ctx, id, f.userID, false); err != ErrRequestNotFound {
		t.Fatalf("deny after approve err = %v, want ErrRequestNotFound", err)
	}
}

func TestDecisionCannotBeMadeOnAnotherUsersRequest(t *testing.T) {
	f := newFixture(t)
	id := location(t, get(t, f.authorizeURL(nil))).Query().Get("request")
	// An unknown user cannot satisfy the active-user lookup.
	if _, err := f.srv.Decide(f.ctx, id, "usr_nobody", true); err != ErrRequestNotFound {
		t.Fatalf("foreign user err = %v, want ErrRequestNotFound", err)
	}
}

func TestServeDecisionRejectsUnauthenticatedCaller(t *testing.T) {
	f := newFixture(t)
	h := f.srv.ServeDecision(func(*http.Request) (string, bool) { return "", false })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("request=x&approve=true"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("decision response not no-store")
	}
}

func TestServeDecisionRejectsUnknownRequestAsNotFound(t *testing.T) {
	f := newFixture(t)
	h := f.srv.ServeDecision(func(*http.Request) (string, bool) { return f.userID, true })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("request=cho_q_missing&approve=true"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestServeDecisionRejectsMalformedBody(t *testing.T) {
	f := newFixture(t)
	h := f.srv.ServeDecision(func(*http.Request) (string, bool) { return f.userID, true })
	rec := httptest.NewRecorder()
	// A lone percent sign is not valid form encoding.
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("request=%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestRevokeRejectsConfidentialCredential(t *testing.T) {
	f := newFixture(t)
	resp, err := http.PostForm(f.http.URL+"/oauth/revoke", url.Values{
		"token": {"cho_a_x"}, "client_id": {testClientID}, "client_secret": {"s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestRevokeRequiresToken(t *testing.T) {
	f := newFixture(t)
	resp, err := http.PostForm(f.http.URL+"/oauth/revoke", url.Values{"client_id": {testClientID}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRevokeMalformedFormIsRejected(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Post(f.http.URL+"/oauth/revoke", "application/x-www-form-urlencoded", strings.NewReader("token=%zz"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRevokeHintedAccessTokenScopesLookup(t *testing.T) {
	f := newFixture(t)
	// A refresh token presented with the access_token hint must still revoke
	// through the refresh-table lookup, since the hint is advisory.
	refresh := f.refreshTokenForDefaultClient(t)
	resp, err := http.PostForm(f.http.URL+"/oauth/revoke", url.Values{
		"token": {refresh}, "client_id": {testClientID}, "token_type_hint": {"access_token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	status, body := f.exchange(url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {testClientID},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("hinted revoke did not revoke: status=%d body=%v", status, body)
	}
}

func TestRevokeHintedRefreshTokenScopesLookup(t *testing.T) {
	f := newFixture(t)
	refresh := f.refreshTokenForDefaultClient(t)
	resp, err := http.PostForm(f.http.URL+"/oauth/revoke", url.Values{
		"token": {refresh}, "client_id": {testClientID}, "token_type_hint": {"refresh_token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	status, _ := f.exchange(url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {testClientID},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("refresh-hinted revoke status = %d", status)
	}
}

func TestClientViewSatisfiesLibraryInterface(t *testing.T) {
	var v clientView
	v.c = Client{ID: testClientID, RedirectURIs: []string{testRedirectURI}}
	if v.GetID() != testClientID || len(v.RedirectURIs()) != 1 {
		t.Fatal("GetID/RedirectURIs")
	}
	if v.PostLogoutRedirectURIs() != nil || v.LoginURL("x") != "" {
		t.Fatal("logout/login surfaces must be empty for a public client")
	}
	if v.AuthMethod() != oidc.AuthMethodNone || v.DevMode() {
		t.Fatal("public client must use auth method none and no dev mode")
	}
	if v.IsScopeAllowed("openid") || v.IDTokenLifetime() != 0 || v.ClockSkew() != 0 {
		t.Fatal("unexpected scope/ID-token/clock policy")
	}
	if v.IDTokenUserinfoClaimsAssertion() || v.RestrictAdditionalIdTokenScopes() != nil || v.RestrictAdditionalAccessTokenScopes() != nil {
		t.Fatal("unexpected token customization")
	}
	if got := v.GrantTypes(); len(got) != 2 {
		t.Fatalf("grant types = %v", got)
	}
	if got := v.ResponseTypes(); len(got) != 1 || got[0] != oidc.ResponseTypeCode {
		t.Fatalf("response types = %v", got)
	}
	if v.ApplicationType() != op.ApplicationTypeNative {
		t.Fatalf("application type = %v, want native (allows loopback http)", v.ApplicationType())
	}
	if v.AccessTokenType() != op.AccessTokenTypeBearer {
		t.Fatalf("access token type = %v, want bearer", v.AccessTokenType())
	}
}

func TestMapDecisionErrTranslatesRaceToNotFound(t *testing.T) {
	if err := mapDecisionErr(storage.ErrOAuthRequestNotDecidable); err != ErrRequestNotFound {
		t.Fatalf("race err = %v, want ErrRequestNotFound", err)
	}
	other := context.Canceled
	if err := mapDecisionErr(other); err != other {
		t.Fatalf("other err changed: %v", err)
	}
}

func TestErrorCodeReadsLibraryErrorCode(t *testing.T) {
	if got := errorCode(oidc.ErrInvalidGrant()); got != "invalid_grant" {
		t.Fatalf("invalid_grant code = %q", got)
	}
	if got := errorCode(oidc.ErrServerError()); got != "server_error" {
		t.Fatalf("server_error code = %q", got)
	}
}

func TestNewOpaqueHasPrefixAndEntropy(t *testing.T) {
	a, err := newOpaque(prefixCode)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newOpaque(prefixCode)
	if !strings.HasPrefix(a, prefixCode) || a == b || len(a) < 40 {
		t.Fatalf("opaque tokens weak or unprefixed: %q %q", a, b)
	}
}
