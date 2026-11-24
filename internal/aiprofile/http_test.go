package aiprofile

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

type fixture struct {
	mux   *http.ServeMux
	store *storage.Store
	svc   *identity.Service
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "profiles.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	svc, err := identity.NewService(store, func() time.Time { return time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	authH := identity.NewHandler(svc)
	mux := http.NewServeMux()
	authH.Register(mux)
	NewHandler(authH, store, func() time.Time { return time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC) }).Register(mux)
	return fixture{mux: mux, store: store, svc: svc}
}

func (f fixture) call(t *testing.T, method, path string, body any, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
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

func validProfile() map[string]any {
	return map[string]any{
		"id":           "alice-live",
		"name":         "Alice Live",
		"protocol":     "live_ws",
		"endpoint_url": "wss://ai.example.test/live",
		"model":        "live-model",
		"voice":        "alloy",
		"instructions": "Be brief.",
		"headers":      map[string]string{"X-Tenant": "alice"},
	}
}

func TestProfileCRUDIsOwnerScoped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Setup(ctx, "root", pwAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateUser(ctx, adminPrincipal(t, f), "alice", pwUser, "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateUser(ctx, adminPrincipal(t, f), "bob", pwUser, "user"); err != nil {
		t.Fatal(err)
	}
	aliceCookie, aliceCSRF := f.login(t, "alice")
	bobCookie, bobCSRF := f.login(t, "bob")

	if rec := f.call(t, http.MethodPost, "/api/ai-profiles", validProfile(), aliceCookie, aliceCSRF); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.call(t, http.MethodGet, "/api/ai-profiles/alice-live", nil, aliceCookie, ""); rec.Code != http.StatusOK {
		t.Fatalf("owner get: %d", rec.Code)
	}
	if rec := f.call(t, http.MethodGet, "/api/ai-profiles/alice-live", nil, bobCookie, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner get: %d, want 404", rec.Code)
	}
	bobList := f.call(t, http.MethodGet, "/api/ai-profiles", nil, bobCookie, "")
	if strings.Contains(bobList.Body.String(), "alice-live") {
		t.Fatalf("bob's list leaked alice's profile: %s", bobList.Body.String())
	}
	upd := validProfile()
	upd["model"] = "hijacked-model"
	if rec := f.call(t, http.MethodPut, "/api/ai-profiles/alice-live", upd, bobCookie, bobCSRF); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner update: %d, want 404", rec.Code)
	}
	alice, err := f.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.store.GetAIProfile(ctx, alice.ID, "alice-live")
	if err != nil || got.Model != "live-model" {
		t.Fatalf("cross-owner update changed profile: %+v, %v", got, err)
	}
	if rec := f.call(t, http.MethodPut, "/api/ai-profiles/alice-live", upd, aliceCookie, aliceCSRF); rec.Code != http.StatusOK {
		t.Fatalf("owner update: %d %s", rec.Code, rec.Body.String())
	}
}

func TestProfileWritesRequireCSRF(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Setup(ctx, "root", pwAdmin); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := f.login(t, "root")
	if rec := f.call(t, http.MethodPost, "/api/ai-profiles", validProfile(), cookie, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF: %d", rec.Code)
	}
	if rec := f.call(t, http.MethodPost, "/api/ai-profiles", validProfile(), cookie, csrf); rec.Code != http.StatusCreated {
		t.Fatalf("valid CSRF: %d %s", rec.Code, rec.Body.String())
	}
}

func TestProfileRejectsUnsafeInput(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Setup(ctx, "root", pwAdmin); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := f.login(t, "root")
	cases := map[string]func(map[string]any){
		"unknown protocol":   func(m map[string]any) { m["protocol"] = "webrtc" },
		"plain remote ws":    func(m map[string]any) { m["endpoint_url"] = "ws://ai.example.test/live" },
		"reserved auth":      func(m map[string]any) { m["headers"] = map[string]string{"Authorization": "Bearer x"} },
		"oversized prompt":   func(m map[string]any) { m["instructions"] = strings.Repeat("a", storage.MaxInstructions+1) },
		"unknown json field": func(m map[string]any) { m["extra"] = true },
	}
	for name, mutate := range cases {
		body := validProfile()
		mutate(body)
		rec := f.call(t, http.MethodPost, "/api/ai-profiles", body, cookie, csrf)
		if rec.Code == http.StatusCreated {
			t.Errorf("%s: created, want rejection", name)
		}
	}
}

func TestProfileSecretReferenceIsOwnerAndPurposeScoped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Setup(ctx, "root", pwAdmin); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"alice", "bob"} {
		if _, err := f.svc.CreateUser(ctx, adminPrincipal(t, f), u, pwUser, "user"); err != nil {
			t.Fatal(err)
		}
	}
	alice, err := f.store.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := f.store.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	seed := func(id, owner, purpose string) {
		t.Helper()
		if err := f.store.InsertSecret(ctx, storage.Secret{
			ID: id, OwnerUserID: owner, Name: id, Purpose: purpose,
			Ciphertext: []byte{1}, Nonce: make([]byte, 12), KeyVersion: 1, CreatedAt: "2026-11-24T10:00:00.000Z",
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("alice-ai-key", alice.ID, "ai_api_key")
	seed("bob-ai-key", bob.ID, "ai_api_key")
	seed("alice-tel-key", alice.ID, "telnyx_api_key")

	aliceCookie, aliceCSRF := f.login(t, "alice")
	bobCookie, bobCSRF := f.login(t, "bob")

	bad := map[string]string{
		"foreign secret": "bob-ai-key",
		"missing secret": "no-such-key",
		"wrong purpose":  "alice-tel-key",
	}
	for name, secretID := range bad {
		body := validProfile()
		body["id"] = "ref-" + strings.ReplaceAll(name, " ", "-")
		body["api_secret_id"] = secretID
		rec := f.call(t, http.MethodPost, "/api/ai-profiles", body, aliceCookie, aliceCSRF)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_input") {
			t.Errorf("create %s: %d %s, want 400 invalid_input", name, rec.Code, rec.Body.String())
		}
	}

	good := validProfile()
	good["id"] = "alice-ref"
	good["api_secret_id"] = "alice-ai-key"
	if rec := f.call(t, http.MethodPost, "/api/ai-profiles", good, aliceCookie, aliceCSRF); rec.Code != http.StatusCreated {
		t.Fatalf("create with own ai key: %d %s", rec.Code, rec.Body.String())
	}

	for name, secretID := range map[string]string{"foreign secret": "bob-ai-key", "missing secret": "no-such-key", "wrong purpose": "alice-tel-key"} {
		body := validProfile()
		body["id"] = "alice-ref"
		body["api_secret_id"] = secretID
		rec := f.call(t, http.MethodPut, "/api/ai-profiles/alice-ref", body, aliceCookie, aliceCSRF)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_input") {
			t.Errorf("update %s: %d %s, want 400 invalid_input", name, rec.Code, rec.Body.String())
		}
	}

	bobBody := validProfile()
	bobBody["id"] = "bob-steal"
	bobBody["api_secret_id"] = "alice-ai-key"
	if rec := f.call(t, http.MethodPost, "/api/ai-profiles", bobBody, bobCookie, bobCSRF); rec.Code != http.StatusBadRequest {
		t.Fatalf("bob binds alice's key: %d, want 400", rec.Code)
	}
}

func TestProfileRoutesRequireSession(t *testing.T) {
	f := newFixture(t)
	if rec := f.call(t, http.MethodGet, "/api/ai-profiles", nil, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list: %d, want 401", rec.Code)
	}
}

func (f fixture) login(t *testing.T, username string) (*http.Cookie, string) {
	t.Helper()
	res, err := f.svc.Login(context.Background(), username, passwordFor(username), "203.0.113.30")
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: identity.SessionCookieName, Value: res.Token}, res.CSRFToken
}

func passwordFor(username string) string {
	if username == "root" {
		return pwAdmin
	}
	return pwUser
}

func adminPrincipal(t *testing.T, f fixture) *identity.Principal {
	t.Helper()
	res, err := f.svc.Login(context.Background(), "root", pwAdmin, "203.0.113.30")
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.svc.Authenticate(context.Background(), res.Token)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
