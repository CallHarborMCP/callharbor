package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

const (
	pwAdmin = "correct horse battery"
	pwUser  = "staple battery horse"
)

type apiFixture struct {
	mux   *http.ServeMux
	svc   *identity.Service
	sec   *Service
	store *storage.Store
}

func newAPIFixture(t *testing.T, keyed bool) apiFixture {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "secrets.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC) }
	idSvc, err := identity.NewService(store, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idSvc.Setup(ctx, "root", pwAdmin); err != nil {
		t.Fatal(err)
	}
	ids := newSeqID()
	var sec *Service
	if keyed {
		box := testBox(t, map[int][]byte{1: testKey(1)}, 1)
		sec, err = NewService(box, store, now, ids)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		sec = NewUnkeyedService(store, now, ids)
	}
	authH := identity.NewHandler(idSvc)
	mux := http.NewServeMux()
	authH.Register(mux)
	NewHandler(authH, sec, store).Register(mux)
	return apiFixture{mux: mux, svc: idSvc, sec: sec, store: store}
}

func newSeqID() func() string {
	n := 0
	return func() string {
		n++
		return "sec_" + strings.Repeat("x", n)
	}
}

// login opens a real identity session and returns the cookie and CSRF token.
func (f apiFixture) login(t *testing.T, user, pw string) (*http.Cookie, string) {
	t.Helper()
	res, err := f.svc.Login(context.Background(), user, pw, "203.0.113.30")
	if err != nil {
		t.Fatalf("login %s: %v", user, err)
	}
	return &http.Cookie{Name: identity.SessionCookieName, Value: res.Token}, res.CSRFToken
}

// addUser creates a standard user through the identity service, as an admin.
func (f apiFixture) addUser(t *testing.T, name string) {
	t.Helper()
	cookie, _ := f.login(t, "root", pwAdmin)
	p, err := f.svc.Authenticate(context.Background(), cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateUser(context.Background(), p, name, pwUser, identity.RoleUser); err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
}

func (f apiFixture) do(t *testing.T, method, path string, body any, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.30:5555"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set(identity.CSRFHeader, csrf)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func TestSecretAPICreateIsWriteOnlyAndNeverReadsBack(t *testing.T) {
	f := newAPIFixture(t, true)
	cookie, csrf := f.login(t, "root", pwAdmin)
	const secretValue = "sk-top-secret-value-9981"

	rec := f.do(t, http.MethodPost, "/api/secrets", map[string]string{"name": "openai", "purpose": "ai_api_key", "value": secretValue}, cookie, csrf)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secretValue) {
		t.Fatal("create response echoed the secret")
	}

	list := f.do(t, http.MethodGet, "/api/secrets", nil, cookie, "")
	if list.Code != http.StatusOK {
		t.Fatalf("list: %d", list.Code)
	}
	if strings.Contains(list.Body.String(), secretValue) {
		t.Fatal("list leaked plaintext")
	}
	for _, forbidden := range []string{"ciphertext", "nonce", "key_version", "value"} {
		if strings.Contains(list.Body.String(), `"`+forbidden+`"`) {
			t.Fatalf("list exposed %q", forbidden)
		}
	}
	var body struct {
		Secrets []map[string]any `json:"secrets"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil || len(body.Secrets) != 1 {
		t.Fatalf("list body: %v %s", err, list.Body.String())
	}
	for _, k := range []string{"id", "name", "purpose", "created_at", "rotated_at"} {
		if _, ok := body.Secrets[0][k]; !ok {
			t.Fatalf("metadata missing %q", k)
		}
	}
}

func TestSecretAPIRequiresCSRFOnWrite(t *testing.T) {
	f := newAPIFixture(t, true)
	cookie, _ := f.login(t, "root", pwAdmin)
	rec := f.do(t, http.MethodPost, "/api/secrets", map[string]string{"name": "k", "purpose": "ai_api_key", "value": "v"}, cookie, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF must be rejected, got %d", rec.Code)
	}
}

func TestSecretAPIRejectsUnauthenticatedAndBadInput(t *testing.T) {
	f := newAPIFixture(t, true)
	if rec := f.do(t, http.MethodGet, "/api/secrets", nil, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: %d", rec.Code)
	}
	cookie, csrf := f.login(t, "root", pwAdmin)
	rec := f.do(t, http.MethodPost, "/api/secrets", map[string]any{"name": "k", "purpose": "ai_api_key", "value": "v", "ciphertext": "x"}, cookie, csrf)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field must be rejected, got %d", rec.Code)
	}
	rec = f.do(t, http.MethodPost, "/api/secrets", map[string]string{"name": "k", "purpose": "not_a_purpose", "value": "v"}, cookie, csrf)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad purpose must be rejected, got %d", rec.Code)
	}
}

func TestSecretAPIFailsClosedWithoutMasterKey(t *testing.T) {
	f := newAPIFixture(t, false)
	cookie, csrf := f.login(t, "root", pwAdmin)
	rec := f.do(t, http.MethodPost, "/api/secrets", map[string]string{"name": "k", "purpose": "ai_api_key", "value": "v"}, cookie, csrf)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "secrets_unavailable") {
		t.Fatalf("write without key must fail closed, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestSecretAPIOwnerIsolation(t *testing.T) {
	ctx := context.Background()
	f := newAPIFixture(t, true)
	f.addUser(t, "alice")
	f.addUser(t, "bob")

	aliceCookie, aliceCSRF := f.login(t, "alice", pwUser)
	const aliceValue = "sk-alice-only-7712"
	rec := f.do(t, http.MethodPost, "/api/secrets", map[string]string{"name": "alice-openai", "purpose": "ai_api_key", "value": aliceValue}, aliceCookie, aliceCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("alice create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("alice create body: %v", err)
	}

	bobCookie, _ := f.login(t, "bob", pwUser)
	bobList := f.do(t, http.MethodGet, "/api/secrets", nil, bobCookie, "")
	if strings.Contains(bobList.Body.String(), created.ID) || strings.Contains(bobList.Body.String(), "alice-openai") {
		t.Fatalf("bob listed alice's secret: %s", bobList.Body.String())
	}

	aliceUser, err := f.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bobUser, err := f.store.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.sec.Resolve(ctx, bobUser.ID, created.ID, PurposeAIAPIKey); err == nil {
		t.Fatal("bob resolved alice's secret")
	}
	got, err := f.sec.Resolve(ctx, aliceUser.ID, created.ID, PurposeAIAPIKey)
	if err != nil || string(got) != aliceValue {
		t.Fatalf("owner must resolve own secret: %v", err)
	}
}

func TestSecretAPINoEndpointReturnsPlaintext(t *testing.T) {
	f := newAPIFixture(t, true)
	cookie, csrf := f.login(t, "root", pwAdmin)
	const value = "sk-never-read-4421"
	f.do(t, http.MethodPost, "/api/secrets", map[string]string{"name": "k", "purpose": "ai_api_key", "value": value}, cookie, csrf)
	for _, path := range []string{"/api/secrets", "/api/secrets/x", "/api/secrets/x/value", "/api/secrets/x/reveal"} {
		rec := f.do(t, http.MethodGet, path, nil, cookie, "")
		if strings.Contains(rec.Body.String(), value) {
			t.Fatalf("GET %s returned plaintext", path)
		}
	}
}
