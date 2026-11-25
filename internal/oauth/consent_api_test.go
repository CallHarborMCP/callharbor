package oauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// consentMux mounts the consent API with the given authenticators.
func (f *fixture) consentMux(read, mutating Authenticator) *http.ServeMux {
	mux := http.NewServeMux()
	f.srv.RegisterConsentAPI(mux, read, mutating)
	return mux
}

// consentView fetches the consent view of request id and decodes it.
func consentView(t *testing.T, f *fixture, id string) map[string]any {
	t.Helper()
	mux := f.consentMux(testAuth(f.userID), testAuth(f.userID))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/oauth/requests/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestConsentViewDomainComesOnlyFromMetadataClientID: a metadata document may
// claim any client_uri. The verified domain is the host the server fetched the
// document from, never the self-declared client_uri host.
func TestConsentViewDomainComesOnlyFromMetadataClientID(t *testing.T) {
	meta := newMetadataServer()
	f, cid := newCIMDFixture(t, meta, true)
	setDoc(meta, cimdDoc(cid, map[string]any{"client_uri": "https://chatgpt.com"}))
	id := location(t, get(t, f.authorizeURL(map[string]string{"client_id": cid}))).Query().Get("request")

	view := consentView(t, f, id)
	wantHost := mustParseHost(t, cid)
	if view["client_host"] != wantHost {
		t.Fatalf("client_host = %v, want the client_id host %q", view["client_host"], wantHost)
	}
	if view["client_uri"] != "https://chatgpt.com" || view["metadata_document"] != true {
		t.Fatalf("view = %v", view)
	}
}

// TestConsentViewHasNoVerifiedDomainForDynamicClient: a dynamically registered
// client has no verified domain, so its client_uri host must not be shown as
// one.
func TestConsentViewHasNoVerifiedDomainForDynamicClient(t *testing.T) {
	f := newFixture(t)
	clientID := f.registerOK(t, `{"redirect_uris":["https://claude.example/cb"],"client_uri":"https://chatgpt.com"}`)
	id := location(t, get(t, f.authorizeURL(map[string]string{
		"client_id":    clientID,
		"redirect_uri": "https://claude.example/cb",
	}))).Query().Get("request")

	view := consentView(t, f, id)
	if view["client_host"] != "" {
		t.Fatalf("client_host = %v, want empty for an unverified client", view["client_host"])
	}
	if view["metadata_document"] != false {
		t.Fatalf("metadata_document = %v, want false", view["metadata_document"])
	}
}

func mustParseHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func TestConsentRequestViewIsReturnedForPendingRequest(t *testing.T) {
	f := newFixture(t)
	id := location(t, get(t, f.authorizeURL(nil))).Query().Get("request")
	mux := f.consentMux(testAuth(f.userID), testAuth(f.userID))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/oauth/requests/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["client_id"] != testClientID || out["resource"] != f.cfg.Resource() || out["metadata_document"] != true {
		t.Fatalf("view = %v", out)
	}
	if out["redirect_uri"] != testRedirectURI {
		t.Fatalf("redirect_uri = %v", out["redirect_uri"])
	}
}

func TestConsentRequestUnknownOrDecidedIs404(t *testing.T) {
	f := newFixture(t)
	id := location(t, get(t, f.authorizeURL(nil))).Query().Get("request")
	mux := f.consentMux(testAuth(f.userID), testAuth(f.userID))
	f.consentFor(id, true)
	for _, path := range []string{"/api/oauth/requests/cho_q_missing", "/api/oauth/requests/" + id} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, rec.Code)
		}
	}
}

// deniedAuth writes the 401 the identity adapter would write, and refuses.
func deniedAuth(w http.ResponseWriter, _ *http.Request) (string, bool) {
	writeNoStore(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
	return "", false
}

func TestConsentRequestNeedsSession(t *testing.T) {
	f := newFixture(t)
	id := location(t, get(t, f.authorizeURL(nil))).Query().Get("request")
	mux := f.consentMux(deniedAuth, deniedAuth)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/oauth/requests/"+id, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestDecisionNeedsCSRF(t *testing.T) {
	f := newFixture(t)
	id := location(t, get(t, f.authorizeURL(nil))).Query().Get("request")
	csrfDenied := func(w http.ResponseWriter, _ *http.Request) (string, bool) {
		writeNoStore(w, http.StatusForbidden, map[string]string{"error": "csrf_invalid"})
		return "", false
	}
	mux := f.consentMux(testAuth(f.userID), csrfDenied)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/oauth/requests/"+id+"/decision", strings.NewReader(`{"approve":true}`))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestGrantsListShowsOnlyLiveGrantsForCaller(t *testing.T) {
	f := newFixture(t)
	f.bearerFixture(t)
	mux := f.consentMux(testAuth(f.userID), testAuth(f.userID))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/oauth/grants", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"client_name"`) || !strings.Contains(rec.Body.String(), `"scopes"`) {
		t.Fatalf("grants body = %s", rec.Body)
	}
	// Another user sees none of this user's grants.
	stranger := f.consentMux(testAuth("usr_someone_else"), testAuth("usr_someone_else"))
	rec = httptest.NewRecorder()
	stranger.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/oauth/grants", nil))
	if strings.Contains(rec.Body.String(), `"client_name"`) {
		t.Fatalf("another user's grants visible: %s", rec.Body)
	}
}

func TestRevokeOwnGrantReturns204AndAudits(t *testing.T) {
	f := newFixture(t)
	_, grantID := f.bearerFixture(t)
	mux := f.consentMux(testAuth(f.userID), testAuth(f.userID))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/oauth/grants/"+grantID, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	n, err := f.store.CountAuditEvents(f.ctx, "oauth.grant_revoked", "success")
	if err != nil || n != 1 {
		t.Fatalf("audit rows = %d err=%v", n, err)
	}
}

func TestRevokeAnotherUsersGrantIs404(t *testing.T) {
	f := newFixture(t)
	_, grantID := f.bearerFixture(t)
	mux := f.consentMux(testAuth("usr_someone_else"), testAuth("usr_someone_else"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/oauth/grants/"+grantID, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	g, err := f.store.GetOAuthGrantByID(f.ctx, grantID)
	if err != nil || g.RevokedAt != "" {
		t.Fatalf("foreign revoke changed the grant: %+v err=%v", g, err)
	}
}
