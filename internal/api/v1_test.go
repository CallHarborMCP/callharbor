package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/service"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephonyprofile"
	_ "modernc.org/sqlite"
)

// fixture builds a real SQLite store with two users, a session for each, and
// REST routes mounted on a mux.
type fixture struct {
	mux     *http.ServeMux
	store   *storage.Store
	aliceCk *http.Cookie
	bobCk   *http.Cookie
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db.sqlite")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Fixture rows go through a second connection to the same file, so the
	// production store API stays free of raw-SQL helpers.
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	idSvc, err := identity.NewService(store, func() time.Time { return now })
	if err != nil {
		t.Fatalf("identity service: %v", err)
	}
	idHandler := identity.NewHandler(idSvc)
	f := &fixture{store: store}
	f.seed(t, raw)
	f.aliceCk = f.login(t, idSvc, "alice")
	f.bobCk = f.login(t, idSvc, "bob")

	ring, err := secrets.NewKeyring(1, map[int][]byte{1: make([]byte, 32)})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	box, err := secrets.NewBox(ring)
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	secSvc, err := secrets.NewService(box, store, func() time.Time { return now }, func() string { return "sec_test" })
	if err != nil {
		t.Fatalf("secrets service: %v", err)
	}
	// The route fixture never reaches validation: its probes are unauthenticated
	// or lack CSRF. The factory refuses rather than dial, so a regression fails.
	telSvc, err := telephonyprofile.New(store, secSvc, func(string) (telephonyprofile.Provider, error) {
		return nil, errors.New("route fixture must not validate against a provider")
	}, func() time.Time { return now }, func() string { return "tp_test" }, nil)
	if err != nil {
		t.Fatalf("telephony service: %v", err)
	}

	f.mux = http.NewServeMux()
	NewHandler(idHandler, service.New(store), telSvc, nil).Register(f.mux)
	return f
}

const seedTime = "2026-11-23T10:00:00.000Z"

// testPassword satisfies identity.ValidatePassword for fixture users.
const testPassword = "correct-horse-battery"

func (f *fixture) seed(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	hash, err := identity.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	stmts := []string{
		`INSERT INTO users (id, username, role, password_hash, active, created_at, updated_at) VALUES
			('alice', 'alice', 'user', '` + hash + `', 1, '` + seedTime + `', '` + seedTime + `'),
			('bob', 'bob', 'user', '` + hash + `', 1, '` + seedTime + `', '` + seedTime + `')`,
		`INSERT INTO secrets (id, owner_user_id, name, purpose, ciphertext, nonce, key_version, created_at) VALUES
			('alice-key', 'alice', 'key', 'telnyx_api_key', x'00', x'00', 1, '` + seedTime + `'),
			('bob-key', 'bob', 'key', 'telnyx_api_key', x'00', x'00', 1, '` + seedTime + `'),
			('alice-ai-key', 'alice', 'ai', 'ai_api_key', x'00', x'00', 1, '` + seedTime + `')`,
		`INSERT INTO telephony_profiles (id, owner_user_id, name, provider, caller_number, api_secret_id, created_at, updated_at) VALUES
			('alice-tel', 'alice', 'main', 'telnyx', '+14155550100', 'alice-key', '` + seedTime + `', '` + seedTime + `'),
			('bob-tel', 'bob', 'main', 'telnyx', '+14155550200', 'bob-key', '` + seedTime + `', '` + seedTime + `')`,
		`INSERT INTO ai_profiles (id, owner_user_id, name, protocol, endpoint_url, model, voice, headers_json, instructions, api_secret_id, created_at, updated_at) VALUES
			('alice-ai', 'alice', 'live', 'live_ws', 'wss://ai.example.test/live', 'model-x', 'alloy', '{"X-Hidden":"yes"}', 'secret instructions', 'alice-ai-key', '` + seedTime + `', '` + seedTime + `')`,
		`INSERT INTO calls (id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at) VALUES
			('alice-call', 'alice', 'alice-tel', 'alice-ai', 'k1', '+14155550111', 'queued', 300, '` + seedTime + `', '` + seedTime + `'),
			('alice-call-2', 'alice', 'alice-tel', 'alice-ai', 'k2', '+14155550112', 'queued', 300, '2026-11-24T10:00:00.000Z', '2026-11-24T10:00:00.000Z')`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
}

func (f *fixture) login(t *testing.T, idSvc *identity.Service, user string) *http.Cookie {
	t.Helper()
	res, err := idSvc.Login(context.Background(), user, testPassword, "203.0.113.7")
	if err != nil {
		t.Fatalf("login %s: %v", user, err)
	}
	return &http.Cookie{Name: identity.SessionCookieName, Value: res.Token}
}

func get(t *testing.T, mux http.Handler, path string, ck *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if ck != nil {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return m
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	m := decodeMap(t, rec)
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope: %s", rec.Body.String())
	}
	if e["message"] == "" || e["message"] == nil {
		t.Fatalf("error message missing: %s", rec.Body.String())
	}
	return e["code"].(string)
}

func TestV1RequiresSessionAndDoesNotLeakOwnership(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{"/api/v1/profiles", "/api/v1/calls", "/api/v1/calls/alice-call"} {
		rec := get(t, f.mux, path, nil)
		if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "unauthenticated" {
			t.Fatalf("%s anonymous = %d %s", path, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s missing no-store", path)
		}
	}
	rec := get(t, f.mux, "/api/v1/calls", &http.Cookie{Name: identity.SessionCookieName, Value: "forged"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged session = %d", rec.Code)
	}
}

func TestV1ProfilesAreOwnerScopedAndNonSecret(t *testing.T) {
	f := newFixture(t)
	rec := get(t, f.mux, "/api/v1/profiles", f.aliceCk)
	if rec.Code != http.StatusOK {
		t.Fatalf("profiles = %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, forbidden := range []string{"bob", "+14155550200", "wss://", "X-Hidden", "secret instructions", "alice-ai-key", "alice-key"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("profiles leak %q: %s", forbidden, body)
		}
	}
	if !strings.Contains(body, `"primary_transport":"websocket"`) || !strings.Contains(body, `"connectivity_status":"unknown"`) {
		t.Fatalf("profiles missing required fields: %s", body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("headers = %v", rec.Header())
	}
}

func TestV1CallsListPaginationAndFilter(t *testing.T) {
	f := newFixture(t)
	rec := get(t, f.mux, "/api/v1/calls?limit=1", f.aliceCk)
	if rec.Code != http.StatusOK {
		t.Fatalf("page1 = %d %s", rec.Code, rec.Body.String())
	}
	page1 := decodeMap(t, rec)
	calls := page1["calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["call_id"] != "alice-call-2" {
		t.Fatalf("page1 calls = %v", calls)
	}
	next, _ := page1["next_cursor"].(string)
	if next == "" {
		t.Fatal("expected next_cursor")
	}
	rec = get(t, f.mux, "/api/v1/calls?limit=1&cursor="+next, f.aliceCk)
	page2 := decodeMap(t, rec)
	if rec.Code != http.StatusOK || len(page2["calls"].([]any)) != 1 || page2["calls"].([]any)[0].(map[string]any)["call_id"] != "alice-call" {
		t.Fatalf("page2 = %d %s", rec.Code, rec.Body.String())
	}
	if page2["next_cursor"] != "" {
		t.Fatalf("last page cursor = %v", page2["next_cursor"])
	}

	rec = get(t, f.mux, "/api/v1/calls?status=ended", f.aliceCk)
	if rec.Code != http.StatusOK || len(decodeMap(t, rec)["calls"].([]any)) != 0 {
		t.Fatalf("status filter = %d %s", rec.Code, rec.Body.String())
	}
	rec = get(t, f.mux, "/api/v1/calls?status=dialing", f.aliceCk)
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_input" {
		t.Fatalf("bad status = %d %s", rec.Code, rec.Body.String())
	}
}

func TestV1CallsRejectBadPaginationInput(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{
		"/api/v1/calls?limit=0",
		"/api/v1/calls?limit=101",
		"/api/v1/calls?limit=abc",
		"/api/v1/calls?cursor=%25%25",
	} {
		rec := get(t, f.mux, path, f.aliceCk)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_input" {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestV1GetCallOwnershipAndShape(t *testing.T) {
	f := newFixture(t)
	rec := get(t, f.mux, "/api/v1/calls/alice-call", f.aliceCk)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body.String())
	}
	m := decodeMap(t, rec)
	if m["call_id"] != "alice-call" || m["technical_status"] != "queued" || m["task_outcome"] != nil {
		t.Fatalf("call body = %v", m)
	}
	for _, key := range []string{"details", "transcript_available", "recording_available"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("missing %s in %v", key, m)
		}
	}

	rec = get(t, f.mux, "/api/v1/calls/alice-call", f.bobCk)
	if rec.Code != http.StatusNotFound || errCode(t, rec) != "not_found" {
		t.Fatalf("cross-user get = %d %s", rec.Code, rec.Body.String())
	}
	rec = get(t, f.mux, "/api/v1/calls/missing", f.aliceCk)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing get = %d", rec.Code)
	}
}

func TestV1MethodsOutsideContractAreNotFound(t *testing.T) {
	f := newFixture(t)
	// GET /api/v1/calls is mounted and POST /api/v1/calls is mounted too. A
	// method outside the contract on a mounted path still answers 405.
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/calls", strings.NewReader("{}"))
	req.AddCookie(f.aliceCk)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /api/v1/calls = %d %s, want 405", rec.Code, rec.Body.String())
	}
}

// TestV1CallsCursorBoundaries covers the REST edge of the cursor rules. A
// tampered or over-length cursor is invalid_input and never reaches the store.
// A missing cursor is the first page.
func TestV1CallsCursorBoundaries(t *testing.T) {
	f := newFixture(t)

	first := get(t, f.mux, "/api/v1/calls", f.aliceCk)
	if first.Code != http.StatusOK {
		t.Fatalf("missing cursor = %d %s", first.Code, first.Body.String())
	}

	over := strings.Repeat("A", service.MaxCursorLength+1)
	rec := get(t, f.mux, "/api/v1/calls?cursor="+over, f.aliceCk)
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_input" {
		t.Fatalf("over-length cursor = %d %s", rec.Code, rec.Body.String())
	}

	tampered := base64url(`{"v":9,"c":"2026-11-23T10:00:00.000Z","i":"alice-call-2"}`)
	rec = get(t, f.mux, "/api/v1/calls?cursor="+tampered, f.aliceCk)
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_input" {
		t.Fatalf("tampered cursor = %d %s", rec.Code, rec.Body.String())
	}
}

// base64url encodes s as an unpadded base64url token, the cursor wire form.
func base64url(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}
