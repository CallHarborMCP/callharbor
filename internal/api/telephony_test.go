package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/service"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/telephonyprofile"
)

// telFixture serves the telephony routes over a real SQLite store, a keyed
// secrets service and a Telnyx REST fake. The fake answers GET only and records
// every request, so tests can prove validation never sends anything else.
type telFixture struct {
	mux    *http.ServeMux
	idSvc  *identity.Service
	db     *sql.DB
	csrf   map[string]string
	cookie map[string]*http.Cookie
	fake   *telnyxFake
}

// telnyxFake serves the two read-only Telnyx endpoints validation reads. It is
// independent of the telephonyprofile fake and of the client's own tests.
type telnyxFake struct {
	mu       sync.Mutex
	validKey string
	apps     map[string]bool   // application id -> active
	numbers  map[string]string // e164 -> status
	numberOn map[string]string // e164 -> connection id
	hits     []string
}

func (f *telnyxFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits = append(f.hits, r.Method+" "+r.URL.Path)
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.validKey {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"detail":"unauthorized"}]}`))
		return
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/v2/call_control_applications/"):
		id := strings.TrimPrefix(r.URL.Path, "/v2/call_control_applications/")
		active, ok := f.apps[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":{"id":%q,"active":%t,"application_name":"app"}}`, id, active)
	case r.URL.Path == "/v2/phone_numbers":
		// Like the pinned contract, a filter with non-numeric characters (such
		// as a leading "+") matches nothing. Digits match the stored E.164
		// number with its "+" removed.
		digits := r.URL.Query().Get("filter[phone_number]")
		if !allDigitsFake(digits) {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		for e164, status := range f.numbers {
			if strings.TrimPrefix(e164, "+") != digits {
				continue
			}
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"num-1","phone_number":%q,"status":%q,"connection_id":%q}]}`, e164, status, f.numberOn[e164])
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// allDigitsFake reports whether s is non-empty and contains only ASCII digits.
func allDigitsFake(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// requests returns a copy of every request the fake has seen.
func (f *telnyxFake) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hits...)
}

// newTelFixture builds the fixture with an admin and two users.
func newTelFixture(t *testing.T) *telFixture {
	t.Helper()
	return newTelFixtureWith(t, true)
}

// newTelFixtureUnkeyed builds the same fixture with no master key configured.
// Secret writes must fail closed, so no profile row can be stored.
func newTelFixtureUnkeyed(t *testing.T) *telFixture {
	t.Helper()
	return newTelFixtureWith(t, false)
}

func newTelFixtureWith(t *testing.T, keyed bool) *telFixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rest.db")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	idSvc, err := identity.NewService(store, func() time.Time { return now })
	if err != nil {
		t.Fatalf("identity: %v", err)
	}

	ring, err := secrets.NewKeyring(1, map[int][]byte{1: make([]byte, 32)})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	box, err := secrets.NewBox(ring)
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	secIDs, profIDs := 0, 0
	newSecID := func() string {
		secIDs++
		return fmt.Sprintf("sec_%d", secIDs)
	}
	var secSvc *secrets.Service
	if keyed {
		secSvc, err = secrets.NewService(box, store, func() time.Time { return now }, newSecID)
		if err != nil {
			t.Fatalf("secrets: %v", err)
		}
	} else {
		secSvc = secrets.NewUnkeyedService(store, func() time.Time { return now }, newSecID)
	}

	fake := &telnyxFake{
		validKey: "KEY-good",
		apps:     map[string]bool{"app-live": true, "app-off": false},
		numbers:  map[string]string{"+14155550100": "active"},
		numberOn: map[string]string{"+14155550100": "app-live"},
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	telSvc, err := telephonyprofile.New(store, secSvc, func(key string) (telephonyprofile.Provider, error) {
		return telnyx.NewClient(srv.URL+"/v2", key, srv.Client())
	}, func() time.Time { return now }, func() string {
		profIDs++
		return fmt.Sprintf("tp_%d", profIDs)
	}, nil)
	if err != nil {
		t.Fatalf("telephony: %v", err)
	}

	f := &telFixture{
		idSvc: idSvc, db: db, fake: fake,
		csrf: map[string]string{}, cookie: map[string]*http.Cookie{},
	}
	seedTelUsers(t, db, now)
	for _, u := range []string{"admin", "alice", "bob"} {
		f.login(t, u)
	}
	f.mux = http.NewServeMux()
	NewHandler(identity.NewHandler(idSvc), service.New(store), telSvc, nil).Register(f.mux)
	return f
}

// seedTelUsers inserts an admin and two users with the shared test password.
func seedTelUsers(t *testing.T, db *sql.DB, now time.Time) {
	t.Helper()
	hash, err := identity.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ts := now.Format("2006-01-02T15:04:05.000Z")
	if _, err := db.Exec(`INSERT INTO users (id, username, role, password_hash, active, created_at, updated_at) VALUES
		('admin', 'admin', 'admin', ?, 1, ?, ?),
		('alice', 'alice', 'user', ?, 1, ?, ?),
		('bob', 'bob', 'user', ?, 1, ?, ?)`,
		hash, ts, ts, hash, ts, ts, hash, ts, ts); err != nil {
		t.Fatalf("seed users: %v", err)
	}
}

// login stores the session cookie and CSRF token for user.
func (f *telFixture) login(t *testing.T, user string) {
	t.Helper()
	res, err := f.idSvc.Login(context.Background(), user, testPassword, "203.0.113.7")
	if err != nil {
		t.Fatalf("login %s: %v", user, err)
	}
	f.cookie[user] = &http.Cookie{Name: identity.SessionCookieName, Value: res.Token}
	f.csrf[user] = res.CSRFToken
}

// do sends a request as user. An empty user sends no cookie. withCSRF attaches
// the user's CSRF token header.
func (f *telFixture) do(t *testing.T, user, method, path string, body any, withCSRF bool) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		return f.send(user, method, path, nil, withCSRF)
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return f.send(user, method, path, b, withCSRF)
}

// doRaw sends an exact JSON text as the body, so a test can send content that
// marshalling would not produce, such as trailing values or explicit nulls.
func (f *telFixture) doRaw(t *testing.T, user, method, path, raw string, withCSRF bool) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(user, method, path, []byte(raw), withCSRF)
}

// send is the shared request path for do and doRaw.
func (f *telFixture) send(user, method, path string, body []byte, withCSRF bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != "" {
		req.AddCookie(f.cookie[user])
		if withCSRF {
			req.Header.Set(identity.CSRFHeader, f.csrf[user])
		}
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func validWrite() map[string]any {
	return map[string]any{
		"name": "main", "caller_number": "+14155550100", "connection_id": "app-live",
		"api_key": "KEY-good",
	}
}

func createTel(t *testing.T, f *telFixture, user string, body map[string]any) string {
	t.Helper()
	rec := f.do(t, user, http.MethodPost, "/api/v1/telephony-profiles", body, true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	return decodeMap(t, rec)["id"].(string)
}

func TestTelephonyCRUDRoundTripNeverReturnsSecret(t *testing.T) {
	f := newTelFixture(t)
	id := createTel(t, f, "alice", validWrite())

	rec := f.do(t, "alice", http.MethodGet, "/api/v1/telephony-profiles/"+id, nil, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, banned := range []string{"KEY-good", "api_key", "api_secret", "sec_", "webhook"} {
		if strings.Contains(body, banned) {
			t.Fatalf("response leaks %q: %s", banned, body)
		}
	}
	got := decodeMap(t, rec)
	if got["connectivity_status"] != "unknown" || got["access"] != "owner" || got["provider"] != "telnyx" {
		t.Fatalf("profile = %v", got)
	}
	if got["connection_id"] != "app-live" {
		t.Fatalf("owner connection_id = %v, want app-live", got["connection_id"])
	}

	list := f.do(t, "alice", http.MethodGet, "/api/v1/telephony-profiles", nil, false)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), id) {
		t.Fatalf("list = %d %s", list.Code, list.Body.String())
	}

	// An omitted api_key on update keeps the stored key, so the profile still
	// validates afterwards with no key re-sent.
	upd := validWrite()
	upd["name"] = "renamed"
	delete(upd, "api_key")
	rec = f.do(t, "alice", http.MethodPut, "/api/v1/telephony-profiles/"+id, upd, true)
	if rec.Code != http.StatusOK || decodeMap(t, rec)["name"] != "renamed" {
		t.Fatalf("update without api_key = %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.do(t, "alice", http.MethodPost, "/api/v1/telephony-profiles/"+id+"/validate", nil, true); rec.Code != http.StatusOK ||
		decodeMap(t, rec)["connectivity_status"] != "verified" {
		t.Fatalf("stored key no longer validates after update: %d %s", rec.Code, rec.Body.String())
	}

	rec = f.do(t, "alice", http.MethodDelete, "/api/v1/telephony-profiles/"+id, nil, true)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	rec = f.do(t, "alice", http.MethodGet, "/api/v1/telephony-profiles/"+id, nil, false)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d", rec.Code)
	}
}

func TestTelephonyWritesRequireCSRF(t *testing.T) {
	f := newTelFixture(t)
	id := createTel(t, f, "alice", validWrite())
	cases := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/telephony-profiles", validWrite()},
		{http.MethodPut, "/api/v1/telephony-profiles/" + id, validWrite()},
		{http.MethodDelete, "/api/v1/telephony-profiles/" + id, nil},
		{http.MethodPost, "/api/v1/telephony-profiles/" + id + "/validate", nil},
		{http.MethodPut, "/api/v1/telephony-profiles/" + id + "/grants/bob", nil},
		{http.MethodDelete, "/api/v1/telephony-profiles/" + id + "/grants/bob", nil},
	}
	for _, c := range cases {
		rec := f.do(t, "admin", c.method, c.path, c.body, false)
		if rec.Code != http.StatusForbidden || errCode(t, rec) != "forbidden" {
			t.Fatalf("%s %s without CSRF = %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

func TestTelephonyAnonymousIsUnauthenticated(t *testing.T) {
	f := newTelFixture(t)
	rec := f.do(t, "", http.MethodGet, "/api/v1/telephony-profiles", nil, false)
	if rec.Code != http.StatusUnauthorized || errCode(t, rec) != "unauthenticated" {
		t.Fatalf("anonymous = %d %s", rec.Code, rec.Body.String())
	}
}

func TestTelephonyCreateRejectsBadBodies(t *testing.T) {
	f := newTelFixture(t)

	unknown := validWrite()
	unknown["owner"] = "bob"
	rec := f.do(t, "alice", http.MethodPost, "/api/v1/telephony-profiles", unknown, true)
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_input" {
		t.Fatalf("unknown field = %d %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/telephony-profiles", strings.NewReader("{}"))
	req.AddCookie(f.cookie["alice"])
	req.Header.Set(identity.CSRFHeader, f.csrf["alice"])
	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_input" {
		t.Fatalf("no content type = %d %s", rec.Code, rec.Body.String())
	}

	badNum := validWrite()
	badNum["caller_number"] = "4155550100"
	rec = f.do(t, "alice", http.MethodPost, "/api/v1/telephony-profiles", badNum, true)
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_input" {
		t.Fatalf("bad number = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "KEY-good") {
		t.Fatalf("error echoes input: %s", rec.Body.String())
	}
}

func TestTelephonyCrossUserIsNotFound(t *testing.T) {
	f := newTelFixture(t)
	id := createTel(t, f, "alice", validWrite())
	base := "/api/v1/telephony-profiles/" + id

	if rec := f.do(t, "bob", http.MethodGet, base, nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("bob get = %d", rec.Code)
	}
	if rec := f.do(t, "bob", http.MethodPut, base, validWrite(), true); rec.Code != http.StatusNotFound {
		t.Fatalf("bob update = %d", rec.Code)
	}
	if rec := f.do(t, "bob", http.MethodDelete, base, nil, true); rec.Code != http.StatusNotFound {
		t.Fatalf("bob delete = %d", rec.Code)
	}
	if rec := f.do(t, "bob", http.MethodPost, base+"/validate", nil, true); rec.Code != http.StatusNotFound {
		t.Fatalf("bob validate = %d", rec.Code)
	}
	if got := f.fake.requests(); len(got) != 0 {
		// Only Alice's own create could reach the fake, and create never validates.
		t.Fatalf("cross-user access reached Telnyx: %v", got)
	}
}

func TestTelephonyValidateReportsReasonsAndIsReadOnly(t *testing.T) {
	f := newTelFixture(t)
	good := createTel(t, f, "alice", validWrite())

	rec := f.do(t, "alice", http.MethodPost, "/api/v1/telephony-profiles/"+good+"/validate", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("validate = %d %s", rec.Code, rec.Body.String())
	}
	if res := decodeMap(t, rec); res["connectivity_status"] != "verified" || res["connectivity_reason"] != "" {
		t.Fatalf("validate result = %v", res)
	}
	for _, hit := range f.fake.requests() {
		if !strings.HasPrefix(hit, "GET ") {
			t.Fatalf("validation sent a non-GET request: %q", hit)
		}
		if strings.Contains(hit, "/calls") {
			t.Fatalf("validation touched a call endpoint: %q", hit)
		}
	}

	badKey := validWrite()
	badKey["api_key"] = "KEY-wrong"
	badKey["name"] = "bad-key"
	wrong := createTel(t, f, "alice", badKey)
	rec = f.do(t, "alice", http.MethodPost, "/api/v1/telephony-profiles/"+wrong+"/validate", nil, true)
	if res := decodeMap(t, rec); res["connectivity_status"] != "failed" || res["connectivity_reason"] != "invalid_credentials" {
		t.Fatalf("wrong key result = %v", res)
	}
	if strings.Contains(rec.Body.String(), "KEY-wrong") {
		t.Fatalf("validation echoes the key: %s", rec.Body.String())
	}

	inactive := validWrite()
	inactive["connection_id"] = "app-off"
	inactive["name"] = "inactive-app"
	off := createTel(t, f, "alice", inactive)
	rec = f.do(t, "alice", http.MethodPost, "/api/v1/telephony-profiles/"+off+"/validate", nil, true)
	if res := decodeMap(t, rec); res["connectivity_reason"] != "application_inactive" {
		t.Fatalf("inactive app = %v", res)
	}

	got := decodeMap(t, f.do(t, "alice", http.MethodGet, "/api/v1/telephony-profiles/"+wrong, nil, false))
	if got["connectivity_status"] != "failed" || got["connectivity_reason"] != "invalid_credentials" {
		t.Fatalf("stored result = %v", got)
	}
}

func TestTelephonyGrantsAreAdminOnlyAndScoped(t *testing.T) {
	f := newTelFixture(t)
	id := createTel(t, f, "admin", validWrite())
	base := "/api/v1/telephony-profiles/" + id

	// Alice is a plain user: the grant gate refuses her before the profile is
	// looked up, so every grant operation is 403.
	if rec := f.do(t, "alice", http.MethodPut, base+"/grants/bob", nil, true); rec.Code != http.StatusForbidden {
		t.Fatalf("user grant = %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.do(t, "alice", http.MethodGet, base+"/grants", nil, false); rec.Code != http.StatusForbidden {
		t.Fatalf("user list grants = %d", rec.Code)
	}
	if rec := f.do(t, "alice", http.MethodDelete, base+"/grants/bob", nil, true); rec.Code != http.StatusForbidden {
		t.Fatalf("user revoke = %d", rec.Code)
	}

	if rec := f.do(t, "admin", http.MethodPut, base+"/grants/bob", nil, true); rec.Code != http.StatusNoContent {
		t.Fatalf("admin grant = %d %s", rec.Code, rec.Body.String())
	}
	bobView := f.do(t, "bob", http.MethodGet, base, nil, false)
	if got := decodeMap(t, bobView); got["access"] != "granted" {
		t.Fatalf("bob access = %v", got)
	} else if _, leaked := got["connection_id"]; leaked {
		t.Fatalf("granted view exposes the owner's connection_id: %s", bobView.Body.String())
	}

	list := f.do(t, "admin", http.MethodGet, base+"/grants", nil, false)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"user_id":"bob"`) {
		t.Fatalf("list grants = %d %s", list.Code, list.Body.String())
	}

	// A grantee is not an admin, so she cannot grant, revoke or list.
	if rec := f.do(t, "bob", http.MethodGet, base+"/grants", nil, false); rec.Code != http.StatusForbidden {
		t.Fatalf("grantee list = %d", rec.Code)
	}
	if rec := f.do(t, "bob", http.MethodPut, base+"/grants/alice", nil, true); rec.Code != http.StatusForbidden {
		t.Fatalf("grantee grant = %d", rec.Code)
	}
	if rec := f.do(t, "bob", http.MethodDelete, base+"/grants/bob", nil, true); rec.Code != http.StatusForbidden {
		t.Fatalf("grantee revoke = %d", rec.Code)
	}

	if rec := f.do(t, "admin", http.MethodDelete, base+"/grants/bob", nil, true); rec.Code != http.StatusNoContent {
		t.Fatalf("admin revoke = %d", rec.Code)
	}
	if rec := f.do(t, "bob", http.MethodGet, base, nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("revoked grantee get = %d", rec.Code)
	}
}

func TestTelephonyGrantRequiresActiveGrantee(t *testing.T) {
	f := newTelFixture(t)
	id := createTel(t, f, "admin", validWrite())
	rec := f.do(t, "admin", http.MethodPut, "/api/v1/telephony-profiles/"+id+"/grants/nobody", nil, true)
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_input" {
		t.Fatalf("unknown grantee = %d %s", rec.Code, rec.Body.String())
	}
}

func TestTelephonyMalformedIDIsNotFound(t *testing.T) {
	f := newTelFixture(t)
	rec := f.do(t, "alice", http.MethodGet, "/api/v1/telephony-profiles/"+strings.Repeat("x", 100), nil, false)
	if rec.Code != http.StatusNotFound || errCode(t, rec) != "not_found" {
		t.Fatalf("long id = %d %s", rec.Code, rec.Body.String())
	}
}

func TestTelephonyDeleteInUseIsConflict(t *testing.T) {
	f := newTelFixture(t)
	id := createTel(t, f, "alice", validWrite())
	ts := "2026-11-25T09:00:00.000Z"
	if _, err := f.db.Exec(`INSERT INTO ai_profiles (id, owner_user_id, name, protocol, endpoint_url, model, voice, headers_json, instructions, api_secret_id, created_at, updated_at) VALUES ('ai-x', 'alice', 'live', 'live_ws', 'wss://ai.example.test/live', 'm', 'v', '{}', '', NULL, ?, ?)`, ts, ts); err != nil {
		t.Fatalf("seed ai profile: %v", err)
	}
	if _, err := f.db.Exec(`INSERT INTO calls (id, owner_user_id, telephony_profile_id, ai_profile_id, idempotency_key, to_number, status, max_duration_seconds, created_at, updated_at) VALUES ('c-x', 'alice', ?, 'ai-x', 'k-x', '+14155550111', 'queued', 300, ?, ?)`, id, ts, ts); err != nil {
		t.Fatalf("seed call: %v", err)
	}
	rec := f.do(t, "alice", http.MethodDelete, "/api/v1/telephony-profiles/"+id, nil, true)
	if rec.Code != http.StatusConflict || errCode(t, rec) != "conflict" {
		t.Fatalf("delete in use = %d %s", rec.Code, rec.Body.String())
	}
}

// TestTelephonyCreateWithoutMasterKeyIs503AndPersistsNothing proves the fail
// closed path end to end: the envelope is 503 with the internal code, the body
// carries no cause or secret, and no profile row or secret row is written.
func TestTelephonyCreateWithoutMasterKeyIs503AndPersistsNothing(t *testing.T) {
	f := newTelFixtureUnkeyed(t)
	rec := f.do(t, "alice", http.MethodPost, "/api/v1/telephony-profiles", validWrite(), true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("create without master key = %d %s, want 503", rec.Code, rec.Body.String())
	}
	if code := errCode(t, rec); code != "internal" {
		t.Fatalf("envelope code = %q, want internal", code)
	}
	if strings.Contains(rec.Body.String(), "KEY-good") {
		t.Fatalf("503 body echoes the key: %s", rec.Body.String())
	}
	var profiles, secretRows int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM telephony_profiles`).Scan(&profiles); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM secrets`).Scan(&secretRows); err != nil {
		t.Fatal(err)
	}
	if profiles != 0 || secretRows != 0 {
		t.Fatalf("fail-closed create persisted rows: profiles=%d secrets=%d", profiles, secretRows)
	}
	if got := f.fake.requests(); len(got) != 0 {
		t.Fatalf("fail-closed create reached Telnyx: %v", got)
	}
}

// TestTelephonyRevokedKeyTurnsVerifiedIntoFailed: a verified profile whose key
// Telnyx revokes is reported failed with invalid_credentials on the next validate.
func TestTelephonyRevokedKeyTurnsVerifiedIntoFailed(t *testing.T) {
	f := newTelFixture(t)
	id := createTel(t, f, "alice", validWrite())
	rec := f.do(t, "alice", http.MethodPost, "/api/v1/telephony-profiles/"+id+"/validate", nil, true)
	if got := decodeMap(t, rec); rec.Code != http.StatusOK || got["connectivity_status"] != "verified" {
		t.Fatalf("first validate = %d %v", rec.Code, got)
	}
	f.fake.validKey = "KEY-revoked-by-telnyx" // gitleaks:allow
	rec = f.do(t, "alice", http.MethodPost, "/api/v1/telephony-profiles/"+id+"/validate", nil, true)
	got := decodeMap(t, rec)
	if rec.Code != http.StatusOK || got["connectivity_status"] != "failed" || got["connectivity_reason"] != "invalid_credentials" {
		t.Fatalf("revoked-key validate = %d %v, want failed/invalid_credentials", rec.Code, got)
	}
	read := decodeMap(t, f.do(t, "alice", http.MethodGet, "/api/v1/telephony-profiles/"+id, nil, false))
	if read["connectivity_status"] != "failed" {
		t.Fatalf("stored status after revocation = %v, want failed", read["connectivity_status"])
	}
}

// TestFailTelephonyRaceIsConflictWithMessage pins the 409 mapping for a validation
// that lost a race with an edit, with its fixed message.
func TestFailTelephonyRaceIsConflictWithMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	h := &Handler{}
	h.failTelephony(rec, telephonyprofile.ErrValidationRace)
	if rec.Code != http.StatusConflict {
		t.Fatalf("race status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "telephony profile changed during validation") {
		t.Fatalf("race body = %s", rec.Body.String())
	}
}

// TestTelephonyRejectsTrailingJSON: one body is one JSON value. A second value
// after the first is an invalid body, not silently ignored.
func TestTelephonyRejectsTrailingJSON(t *testing.T) {
	f := newTelFixture(t)
	valid := `{"name":"main","caller_number":"+14155550100","connection_id":"app-live","api_key":"KEY-good"}` // gitleaks:allow
	for _, body := range []string{valid + `{"name":"x"}`, valid + `}`, valid + `]`} {
		rec := f.doRaw(t, "alice", http.MethodPost, "/api/v1/telephony-profiles", body, true)
		if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_input" {
			t.Fatalf("trailing content %q = %d %s, want 400 invalid_input", body[len(valid):], rec.Code, rec.Body.String())
		}
	}
	// Whitespace after the value is not content: the same body is accepted.
	rec := f.doRaw(t, "alice", http.MethodPost, "/api/v1/telephony-profiles", valid+" \n\t ", true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("trailing whitespace = %d %s, want 201", rec.Code, rec.Body.String())
	}
}

// TestTelephonyWebhookKeyNullRemovesAndEmptyKeeps covers the update semantics of
// webhook_public_key: null removes the stored key, an empty string keeps it, and
// an absent field keeps it. Removal also deletes the sealed secret row.
func TestTelephonyWebhookKeyNullRemovesAndEmptyKeeps(t *testing.T) {
	f := newTelFixture(t)
	const webhook = "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=" // gitleaks:allow
	create := validWrite()
	create["webhook_public_key"] = webhook
	id := createTel(t, f, "alice", create)

	countWebhookSecrets := func() int {
		var n int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM secrets WHERE purpose = 'telnyx_webhook_public_key'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if countWebhookSecrets() != 1 {
		t.Fatalf("webhook secrets after create = %d, want 1", countWebhookSecrets())
	}

	update := func(raw string) {
		t.Helper()
		rec := f.doRaw(t, "alice", http.MethodPut, "/api/v1/telephony-profiles/"+id, raw, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("update %s = %d %s", raw, rec.Code, rec.Body.String())
		}
	}
	// Empty string keeps the stored key.
	update(`{"name":"main","caller_number":"+14155550100","connection_id":"app-live","webhook_public_key":""}`)
	if countWebhookSecrets() != 1 {
		t.Fatalf("empty string removed the webhook key")
	}
	// Absent keeps the stored key.
	update(`{"name":"main","caller_number":"+14155550100","connection_id":"app-live"}`)
	if countWebhookSecrets() != 1 {
		t.Fatalf("absent field removed the webhook key")
	}
	// Explicit null removes it and deletes the sealed row.
	update(`{"name":"main","caller_number":"+14155550100","connection_id":"app-live","webhook_public_key":null}`)
	if countWebhookSecrets() != 0 {
		t.Fatalf("explicit null did not remove the webhook secret row")
	}
	// On create, null has no stored key to remove, so it is refused.
	rec := f.doRaw(t, "alice", http.MethodPost, "/api/v1/telephony-profiles",
		`{"name":"other","caller_number":"+14155550101","connection_id":"app-live","api_key":"KEY-good","webhook_public_key":null}`, true) // gitleaks:allow
	if rec.Code != http.StatusBadRequest || errCode(t, rec) != "invalid_input" {
		t.Fatalf("create with null webhook = %d %s, want 400 invalid_input", rec.Code, rec.Body.String())
	}
}
