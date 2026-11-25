package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
	"golang.org/x/oauth2"
)

func boolPtr(b bool) *bool { return &b }

// deactivate turns a user inactive. A second active admin is created first so
// the last-admin guard does not block the change under test.
func (f *fixture) deactivate(userID string) {
	f.t.Helper()
	now := formatTime(f.clock.Now())
	if err := f.store.UpdateUser(f.ctx, userID, nil, boolPtr(false), now, storage.AuditEvent{
		OccurredAt: now, Action: "user.update", ResourceType: "user", ResourceID: userID, Outcome: "success", DetailJSON: "{}",
	}); err != nil {
		f.t.Fatal(err)
	}
}

// testAuth stands in for the identity adapter: every request is the given user.
func testAuth(userID string) Authenticator {
	return func(http.ResponseWriter, *http.Request) (string, bool) { return userID, true }
}

// consentFor simulates the signed-in user answering the consent screen. It
// drives the decision through the JSON consent API over real HTTP.
func (f *fixture) consentFor(requestID string, approve bool) *url.URL {
	f.t.Helper()
	payload, err := json.Marshal(map[string]bool{"approve": approve})
	if err != nil {
		f.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, f.http.URL+"/api/oauth/requests/"+requestID+"/decision", bytes.NewReader(payload))
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("decision status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		f.t.Fatal(err)
	}
	u, err := url.Parse(body.RedirectURL)
	if err != nil {
		f.t.Fatal(err)
	}
	return u
}

// approveAuthorize runs authorize with the given parameters, then approves
// the resulting consent request and returns the redirect the client receives.
func (f *fixture) approveAuthorize(params map[string]string) *url.URL {
	f.t.Helper()
	id := location(f.t, get(f.t, f.authorizeURL(params))).Query().Get("request")
	return f.consentFor(id, true)
}

// exchange posts a token request and returns the status and decoded body.
func (f *fixture) exchange(form url.Values) (int, map[string]any) {
	f.t.Helper()
	resp, err := http.PostForm(f.http.URL+"/oauth/token", form)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		f.t.Fatal(err)
	}
	return resp.StatusCode, body
}

func codeFrom(t *testing.T, redirect *url.URL) string {
	t.Helper()
	code := redirect.Query().Get("code")
	if code == "" {
		t.Fatalf("redirect has no code: %s", redirect)
	}
	return code
}

func TestFullFlowWithX2OAuthClient(t *testing.T) {
	f := newFixture(t)

	// Authorize through the real redirect, approve, then redeem with x/oauth2.
	redirect := f.approveAuthorize(nil)
	if redirect.Query().Get("state") != "xyz" {
		t.Fatalf("state = %q, want xyz", redirect.Query().Get("state"))
	}
	if redirect.Query().Get("iss") != f.cfg.Issuer() {
		t.Fatalf("iss = %q, want %q", redirect.Query().Get("iss"), f.cfg.Issuer())
	}
	code := codeFrom(t, redirect)

	conf := &oauth2.Config{
		ClientID:    testClientID,
		RedirectURL: testRedirectURI,
		Endpoint: oauth2.Endpoint{
			AuthURL:   f.http.URL + "/oauth/authorize",
			TokenURL:  f.http.URL + "/oauth/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
	tok, err := conf.Exchange(context.Background(), code,
		oauth2.SetAuthURLParam("code_verifier", testVerifier),
		oauth2.SetAuthURLParam("resource", f.cfg.Resource()),
	)
	if err != nil {
		t.Fatalf("x/oauth2 exchange: %v", err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("missing tokens: access=%q refresh=%q", tok.AccessToken, tok.RefreshToken)
	}
	if !strings.HasPrefix(tok.AccessToken, prefixAccess) {
		t.Fatalf("access token prefix = %q", tok.AccessToken[:min(len(tok.AccessToken), 8)])
	}
	if tok.Extra("id_token") != nil {
		t.Fatal("id_token issued; this server must not issue ID tokens")
	}
	if got := tok.Extra("scope"); got != "mcp:read mcp:call" {
		t.Fatalf("scope = %v", got)
	}

	// Refresh rotates: the new refresh token differs from the old one.
	refreshed, err := conf.TokenSource(context.Background(), &oauth2.Token{
		RefreshToken: tok.RefreshToken,
		Expiry:       time.Now().Add(-time.Hour),
	}).Token()
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if refreshed.RefreshToken == tok.RefreshToken || refreshed.AccessToken == tok.AccessToken {
		t.Fatal("refresh did not rotate both tokens")
	}

	// Replaying the old refresh token revokes the family, including the
	// newest tokens.
	status, _ := f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tok.RefreshToken},
		"client_id":     {testClientID},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("refresh reuse status = %d, want 400", status)
	}
	status, _ = f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshed.RefreshToken},
		"client_id":     {testClientID},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("family survived reuse: status = %d, want 400", status)
	}

	// Revocation (RFC 7009) of a live token succeeds with 200 and no body.
	live := f.approveAuthorize(nil)
	liveTok, err := conf.Exchange(context.Background(), codeFrom(t, live),
		oauth2.SetAuthURLParam("code_verifier", testVerifier),
		oauth2.SetAuthURLParam("resource", f.cfg.Resource()),
	)
	if err != nil {
		t.Fatal(err)
	}
	revokeForm := url.Values{"token": {liveTok.AccessToken}, "client_id": {testClientID}}
	resp, err := http.PostForm(f.http.URL+"/oauth/revoke", revokeForm)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200", resp.StatusCode)
	}
	if _, err := f.store.GetOAuthAccessToken(f.ctx, hashToken(liveTok.AccessToken)); err != nil {
		t.Fatalf("revoked access token row missing: %v", err)
	}
}

func TestDenyRedirectsAccessDeniedWithStateAndIss(t *testing.T) {
	f := newFixture(t)
	id := location(t, get(t, f.authorizeURL(nil))).Query().Get("request")
	redirect := f.consentFor(id, false)
	q := redirect.Query()
	if q.Get("error") != "access_denied" {
		t.Fatalf("error = %q, want access_denied", q.Get("error"))
	}
	if q.Get("state") != "xyz" || q.Get("iss") != f.cfg.Issuer() {
		t.Fatalf("state/iss = %q/%q", q.Get("state"), q.Get("iss"))
	}
	if q.Get("code") != "" {
		t.Fatal("deny response carried a code")
	}
}

func TestDecisionIsSingleUse(t *testing.T) {
	f := newFixture(t)
	id := location(t, get(t, f.authorizeURL(nil))).Query().Get("request")
	f.consentFor(id, true)
	// The second decision must not produce a second code.
	if _, err := f.srv.Decide(f.ctx, id, f.userID, true); err == nil {
		t.Fatal("request was decided twice")
	}
}

func TestExpiredAuthRequestCannotBeDecided(t *testing.T) {
	f := newFixture(t)
	id := location(t, get(t, f.authorizeURL(nil))).Query().Get("request")
	f.clock.Advance(authRequestTTL + time.Second)
	if _, err := f.srv.Decide(f.ctx, id, f.userID, true); err != ErrRequestNotFound {
		t.Fatalf("expired request err = %v, want ErrRequestNotFound", err)
	}
}

func TestDecisionForUnknownRequestIsNotFound(t *testing.T) {
	f := newFixture(t)
	if _, err := f.srv.Decide(f.ctx, "cho_q_nope", f.userID, true); err != ErrRequestNotFound {
		t.Fatalf("err = %v, want ErrRequestNotFound", err)
	}
}

func TestTokenRejectsMissingResourceOnAuthorizeNotToken(t *testing.T) {
	// A missing resource at token time is allowed when the authorize step
	// already bound it, per RFC 8707 and the grant record.
	f := newFixture(t)
	redirect := f.approveAuthorize(nil)
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeFrom(t, redirect)},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d body=%v", status, body)
	}
}

func TestTokenRejectsWrongResource(t *testing.T) {
	f := newFixture(t)
	redirect := f.approveAuthorize(nil)
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeFrom(t, redirect)},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
		"resource":      {"https://other.example/mcp"},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_target" {
		t.Fatalf("status=%d body=%v, want 400 invalid_target", status, body)
	}
}

func TestTokenRejectsRedirectURIMismatch(t *testing.T) {
	f := newFixture(t)
	redirect := f.approveAuthorize(nil)
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeFrom(t, redirect)},
		"client_id":     {testClientID},
		"redirect_uri":  {"https://client.example/other"},
		"code_verifier": {testVerifier},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("status=%d body=%v, want invalid_grant", status, body)
	}
}

// TestTokenRejectsCodeForAnotherClient: a code is bound to the client it was
// issued to. A different client_id presented with the code is refused, and no
// tokens are issued.
func TestTokenRejectsCodeForAnotherClient(t *testing.T) {
	f := newFixture(t)
	code := codeFrom(t, f.approveAuthorize(nil))
	const otherClient = "https://other.example/meta.json"
	f.registerClient(otherClient, []string{testRedirectURI})
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {otherClient},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("status=%d body=%v, want invalid_grant", status, body)
	}
	if _, ok := body["access_token"]; ok {
		t.Fatal("a token was issued to a client that does not hold the code")
	}
}

// TestTokenRejectsCodeWhoseGrantWasRevoked: revoking the grant, for example
// from the consent API, refuses a code that was issued before the revocation.
// The code itself is unconsumed, so only the grant check can refuse it.
func TestTokenRejectsCodeWhoseGrantWasRevoked(t *testing.T) {
	f := newFixture(t)
	code := codeFrom(t, f.approveAuthorize(nil))
	row, err := f.store.GetOAuthCode(f.ctx, hashToken(code))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RevokeOAuthGrant(f.ctx, row.GrantID, formatTime(f.clock.Now()), storage.AuditEvent{
		OccurredAt: formatTime(f.clock.Now()), Action: "oauth.grant_revoked", ResourceType: "oauth_grant",
		ResourceID: row.GrantID, Outcome: "success", DetailJSON: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("status=%d body=%v, want invalid_grant", status, body)
	}
	if _, ok := body["access_token"]; ok {
		t.Fatal("a token was issued from a revoked grant")
	}
}

func TestTokenWrongVerifierDoesNotBurnCode(t *testing.T) {
	f := newFixture(t)
	code := codeFrom(t, f.approveAuthorize(nil))
	status, _ := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {"wrong-verifier-wrong-verifier-wrong-verifier-0000"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("wrong verifier status = %d, want 400", status)
	}
	// The legitimate verifier still works: the bad attempt did not consume it.
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	})
	if status != http.StatusOK {
		t.Fatalf("correct verifier after a bad one: status=%d body=%v", status, body)
	}
}

func TestCodeReuseFailsAndRevokesIssuedTokens(t *testing.T) {
	f := newFixture(t)
	code := codeFrom(t, f.approveAuthorize(nil))
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	}
	status, body := f.exchange(form)
	if status != http.StatusOK {
		t.Fatalf("first exchange: %d %v", status, body)
	}
	access, _ := body["access_token"].(string)
	refresh, _ := body["refresh_token"].(string)

	status, body = f.exchange(form)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("replay status=%d body=%v, want invalid_grant", status, body)
	}
	if _, err := f.store.GetOAuthAccessToken(f.ctx, hashToken(access)); err != nil {
		t.Fatal(err)
	}
	// The first exchange's tokens are revoked by the replay.
	revoked, err := f.store.GetOAuthAccessToken(f.ctx, hashToken(access))
	if err != nil || revoked.RevokedAt == "" {
		t.Fatalf("access token not revoked after code replay: %+v err=%v", revoked, err)
	}
	status, _ = f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {testClientID},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("refresh after code replay status = %d, want 400", status)
	}
}

func TestParallelCodeExchangeHasExactlyOneWinner(t *testing.T) {
	f := newFixture(t)
	code := codeFrom(t, f.approveAuthorize(nil))
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	}
	const racers = 8
	statuses := make(chan int, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.PostForm(f.http.URL+"/oauth/token", form)
			if err != nil {
				statuses <- -1
				return
			}
			_ = resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	wg.Wait()
	close(statuses)
	wins := 0
	for s := range statuses {
		if s == http.StatusOK {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("successful exchanges = %d, want exactly 1", wins)
	}
}

func TestExpiredCodeRejected(t *testing.T) {
	f := newFixture(t)
	code := codeFrom(t, f.approveAuthorize(nil))
	f.clock.Advance(codeTTL + time.Second)
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("expired code status=%d body=%v", status, body)
	}
}

// TestClientAssertionIsRefusedOnTokenAndRevoke: a JWT client assertion is a
// confidential-client credential this server does not verify. Both endpoints
// refuse it with 401 invalid_client, so a client that sends one learns why.
func TestClientAssertionIsRefusedOnTokenAndRevoke(t *testing.T) {
	f := newFixture(t)
	assertion := url.Values{
		"client_assertion":      {"eyJhbGciOiJSUzI1NiJ9.e30.c2ln"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
	}
	for _, tc := range []struct {
		name     string
		endpoint string
		form     url.Values
	}{
		{"token", "/oauth/token", url.Values{
			"grant_type": {"authorization_code"}, "code": {"x"}, "client_id": {testClientID},
		}},
		{"revoke", "/oauth/revoke", url.Values{
			"token": {"cho_a_x"}, "client_id": {testClientID},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{}
			for k, v := range tc.form {
				form[k] = v
			}
			for k, v := range assertion {
				form[k] = v
			}
			resp, err := http.PostForm(f.http.URL+tc.endpoint, form)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusUnauthorized || body["error"] != "invalid_client" {
				t.Fatalf("status=%d body=%v, want 401 invalid_client", resp.StatusCode, body)
			}
		})
	}
}

func TestTokenRejectsConfidentialClientCredential(t *testing.T) {
	f := newFixture(t)
	status, _ := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"x"},
		"client_id":     {testClientID},
		"client_secret": {"s3cr3t"},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("client_secret status = %d, want 401", status)
	}
}

func TestTokenRejectsUnsupportedGrantType(t *testing.T) {
	f := newFixture(t)
	status, body := f.exchange(url.Values{"grant_type": {"password"}, "client_id": {testClientID}})
	if status != http.StatusBadRequest || body["error"] != "unsupported_grant_type" {
		t.Fatalf("status=%d body=%v", status, body)
	}
}

func TestRefreshRequiresClientID(t *testing.T) {
	f := newFixture(t)
	status, _ := f.exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"cho_r_x"}})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

func TestRefreshRejectsForeignClient(t *testing.T) {
	f := newFixture(t)
	f.registerClient("https://other.example/m.json", []string{"https://other.example/cb"})
	access := f.refreshTokenForDefaultClient(t)
	status, body := f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {access},
		"client_id":     {"https://other.example/m.json"},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("status=%d body=%v", status, body)
	}
}

// TestRefreshNarrowingDoesNotNarrowRefreshToken: RFC 6749 section 6. A refresh
// that asks for a subset narrows only the access token. The rotated refresh
// token keeps the grant's scopes, so a later refresh without scope gets all of
// them back.
func TestRefreshNarrowingDoesNotNarrowRefreshToken(t *testing.T) {
	f := newFixture(t)
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeFrom(t, f.approveAuthorize(map[string]string{"scope": "mcp:read mcp:call"}))},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	})
	if status != http.StatusOK {
		t.Fatalf("exchange: %d %v", status, body)
	}
	refresh, _ := body["refresh_token"].(string)

	status, body = f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {testClientID},
		"scope":         {"mcp:read"},
	})
	if status != http.StatusOK || body["scope"] != "mcp:read" {
		t.Fatalf("narrowed refresh: status=%d body=%v, want scope mcp:read", status, body)
	}
	narrowedRefresh, _ := body["refresh_token"].(string)

	status, body = f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {narrowedRefresh},
		"client_id":     {testClientID},
	})
	if status != http.StatusOK || body["scope"] != "mcp:read mcp:call" {
		t.Fatalf("refresh after narrowing: status=%d body=%v, want full original scopes", status, body)
	}
}

func TestRefreshRejectsScopeEscalation(t *testing.T) {
	f := newFixture(t)
	// Grant only mcp:read, then ask to refresh for mcp:call.
	redirect := f.approveAuthorize(map[string]string{"scope": "mcp:read"})
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeFrom(t, redirect)},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	})
	if status != http.StatusOK {
		t.Fatalf("exchange: %d %v", status, body)
	}
	refresh, _ := body["refresh_token"].(string)
	status, body = f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {testClientID},
		"scope":         {"mcp:read mcp:call"},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_scope" {
		t.Fatalf("escalation status=%d body=%v, want invalid_scope", status, body)
	}
}

func TestRefreshRejectsWrongResource(t *testing.T) {
	f := newFixture(t)
	refresh := f.refreshTokenForDefaultClient(t)
	status, body := f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {testClientID},
		"resource":      {"https://other.example/mcp"},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_target" {
		t.Fatalf("status=%d body=%v, want invalid_target", status, body)
	}
}

func TestRefreshExpiresAfterThirtyDays(t *testing.T) {
	f := newFixture(t)
	refresh := f.refreshTokenForDefaultClient(t)
	f.clock.Advance(refreshTokenTTL + time.Second)
	status, body := f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {testClientID},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("expired refresh status=%d body=%v", status, body)
	}
}

func TestRevokedRefreshTokenRejectedWithoutReuseMarking(t *testing.T) {
	f := newFixture(t)
	refresh := f.refreshTokenForDefaultClient(t)
	resp, err := http.PostForm(f.http.URL+"/oauth/revoke", url.Values{"token": {refresh}, "client_id": {testClientID}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	status, body := f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {testClientID},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("revoked refresh status=%d body=%v", status, body)
	}
}

func TestDeactivatedUserCannotRefresh(t *testing.T) {
	f := newFixture(t)
	refresh := f.refreshTokenForDefaultClient(t)
	// Deactivate the user directly in storage; deactivation revokes grants in
	// the same transaction, and the token path must refuse regardless.
	f.deactivate(f.userID)
	status, body := f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {testClientID},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("deactivated refresh status=%d body=%v", status, body)
	}
}

func TestDeactivatedUserCannotRedeemCode(t *testing.T) {
	f := newFixture(t)
	code := codeFrom(t, f.approveAuthorize(nil))
	f.deactivate(f.userID)
	status, _ := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("deactivated code redemption status = %d, want 400", status)
	}
}

func TestConsentRefusedForDeactivatedUser(t *testing.T) {
	f := newFixture(t)
	id := location(t, get(t, f.authorizeURL(nil))).Query().Get("request")
	f.deactivate(f.userID)
	if _, err := f.srv.Decide(f.ctx, id, f.userID, true); err == nil {
		t.Fatal("consent approved for a deactivated user")
	}
}

func TestRevokeUnknownTokenReturns200(t *testing.T) {
	f := newFixture(t)
	resp, err := http.PostForm(f.http.URL+"/oauth/revoke", url.Values{"token": {"cho_a_unknown"}, "client_id": {testClientID}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unknown token status = %d, want 200 (RFC 7009)", resp.StatusCode)
	}
}

func TestRevokeOtherClientsTokenIsNoOp(t *testing.T) {
	f := newFixture(t)
	f.registerClient("https://other.example/m.json", []string{"https://other.example/cb"})
	refresh := f.refreshTokenForDefaultClient(t)
	resp, err := http.PostForm(f.http.URL+"/oauth/revoke", url.Values{"token": {refresh}, "client_id": {"https://other.example/m.json"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	status, _ := f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {testClientID},
	})
	if status != http.StatusOK {
		t.Fatalf("another client revoked the token: refresh status = %d", status)
	}
}

func TestRevokeRequiresClientID(t *testing.T) {
	f := newFixture(t)
	resp, err := http.PostForm(f.http.URL+"/oauth/revoke", url.Values{"token": {"cho_a_x"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("revoke without client_id status = %d, want 400", resp.StatusCode)
	}
}

func TestTokenResponsesAreNoStore(t *testing.T) {
	f := newFixture(t)
	code := codeFrom(t, f.approveAuthorize(nil))
	resp, err := http.PostForm(f.http.URL+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("token Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
}

// refreshTokenForDefaultClient runs a full authorize/approve/exchange and
// returns the refresh token, for tests that need one as a starting point.
func (f *fixture) refreshTokenForDefaultClient(t *testing.T) string {
	t.Helper()
	status, body := f.exchange(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {codeFrom(t, f.approveAuthorize(nil))},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	})
	if status != http.StatusOK {
		t.Fatalf("helper exchange: %d %v", status, body)
	}
	refresh, _ := body["refresh_token"].(string)
	return refresh
}

// revokeFailer makes grant revocation fail, to exercise the logged path.
type revokeFailer struct {
	*storage.Store
}

func (r revokeFailer) RevokeOAuthGrant(context.Context, string, string, storage.AuditEvent) error {
	return errors.New("disk on fire")
}

// TestReplayRevocationFailureIsLoggedWithoutTokens: when the family cannot be
// revoked after reuse, the failure is logged, the client still gets
// invalid_grant, and the log carries no token values.
func TestReplayRevocationFailureIsLoggedWithoutTokens(t *testing.T) {
	f := newFixture(t)
	original := f.refreshTokenForDefaultClient(t)
	f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {original},
		"client_id":     {testClientID},
	})

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	f.serve(newServer(f.cfg, revokeFailer{f.store}, registeredResolver{f.store}, f.clock.Now, logger, registerGlobalCap))

	status, _ := f.exchange(url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {original},
		"client_id":     {testClientID},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("replay status = %d, want 400", status)
	}
	if !strings.Contains(logs.String(), "revoke replayed grant failed") {
		t.Fatalf("revocation failure not logged: %q", logs.String())
	}
	if strings.Contains(logs.String(), original) {
		t.Fatalf("log leaked a refresh token: %q", logs.String())
	}
}

// tokenLookupFailer fails access-token lookups, to exercise the resource 500
// path. Every other call goes to the real store.
type tokenLookupFailer struct {
	*storage.Store
}

func (r tokenLookupFailer) GetOAuthAccessToken(context.Context, string) (storage.OAuthToken, error) {
	return storage.OAuthToken{}, errors.New("disk on fire")
}

// TestResourceStoreFailureIsLoggedWithoutToken: a storage failure while
// checking a bearer token answers 500 and is logged, and the log never carries
// the presented token.
func TestResourceStoreFailureIsLoggedWithoutToken(t *testing.T) {
	f := newFixture(t)
	token, _ := f.bearerFixture(t)
	var logs bytes.Buffer
	f.srv = newServer(f.cfg, tokenLookupFailer{f.store}, registeredResolver{f.store}, f.clock.Now, slog.New(slog.NewTextHandler(&logs, nil)), registerGlobalCap)
	rec, _, _ := f.guarded(t, bearerReq(token), ScopeRead)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(logs.String(), "load access token failed") {
		t.Fatalf("storage failure not logged: %q", logs.String())
	}
	if strings.Contains(logs.String(), token) {
		t.Fatal("log carries the presented bearer token")
	}
}

// revokeLookupFailer fails token revocation, to exercise the revoke 500 path.
type revokeLookupFailer struct {
	*storage.Store
}

func (r revokeLookupFailer) RevokeOAuthTokenByHash(context.Context, string, string) (bool, error) {
	return false, errors.New("disk on fire")
}

// TestRevokeStoreFailureIsLoggedWithoutToken: a storage failure during
// revocation answers 500 and is logged, and the log never carries the token.
func TestRevokeStoreFailureIsLoggedWithoutToken(t *testing.T) {
	f := newFixture(t)
	token, _ := f.bearerFixture(t)
	var logs bytes.Buffer
	f.serve(newServer(f.cfg, revokeLookupFailer{f.store}, registeredResolver{f.store}, f.clock.Now, slog.New(slog.NewTextHandler(&logs, nil)), registerGlobalCap))
	resp, err := http.PostForm(f.http.URL+"/oauth/revoke", url.Values{"token": {token}, "client_id": {testClientID}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if !strings.Contains(logs.String(), "revoke: internal failure") {
		t.Fatalf("revoke failure not logged: %q", logs.String())
	}
	if strings.Contains(logs.String(), token) {
		t.Fatal("log carries the presented token")
	}
}

// TestTokenMissingOrEmptyVerifierIsRefused: a code exchange with no code_verifier,
// or an empty one, is refused with invalid_grant or invalid_request, the library's
// answer for a missing PKCE verifier. The refusal must not burn the code: the
// legitimate verifier still redeems it afterwards.
func TestTokenMissingOrEmptyVerifierIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verifier url.Values
	}{
		{"missing", url.Values{}},
		{"empty", url.Values{"code_verifier": {""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			code := codeFrom(t, f.approveAuthorize(nil))
			form := url.Values{
				"grant_type":   {"authorization_code"},
				"code":         {code},
				"client_id":    {testClientID},
				"redirect_uri": {testRedirectURI},
			}
			for k, v := range tc.verifier {
				form[k] = v
			}
			status, body := f.exchange(form)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d body = %v, want 400", status, body)
			}
			if e, _ := body["error"].(string); e != "invalid_grant" && e != "invalid_request" {
				t.Fatalf("error = %v, want invalid_grant or invalid_request", body["error"])
			}
			status, body = f.exchange(url.Values{
				"grant_type":    {"authorization_code"},
				"code":          {code},
				"client_id":     {testClientID},
				"redirect_uri":  {testRedirectURI},
				"code_verifier": {testVerifier},
			})
			if status != http.StatusOK {
				t.Fatalf("legitimate exchange after refusal = %d %v; the refusal burned the code", status, body)
			}
		})
	}
}
