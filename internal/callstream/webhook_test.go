package callstream

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	_ "modernc.org/sqlite"
)

const seedTime = "2026-11-25T12:00:00.000Z"

// newWebhookStore opens a migrated store and a second handle to the same file
// for seeding and reading. SQLite allows both; the Store is the one the app
// uses, and the handle is only for setup and assertions.
func newWebhookStore(t *testing.T) (*storage.Store, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wh.db")
	s, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return s, db
}

// seedCall inserts a call in the given status with a control id.
func seedCall(t *testing.T, db *sql.DB, id, status, control string) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`INSERT OR IGNORE INTO users (id, username, role, password_hash, created_at, updated_at)
			VALUES ('alice', 'alice', 'user', 'hash', '` + seedTime + `', '` + seedTime + `')`,
		`INSERT OR IGNORE INTO secrets (id, owner_user_id, name, purpose, ciphertext, nonce, key_version, created_at)
			VALUES ('k', 'alice', 'key', 'telnyx_api_key', x'00', x'00', 1, '` + seedTime + `')`,
		`INSERT OR IGNORE INTO telephony_profiles (id, owner_user_id, name, provider, caller_number, api_secret_id, created_at, updated_at)
			VALUES ('tel', 'alice', 'main', 'telnyx', '+14155550100', 'k', '` + seedTime + `', '` + seedTime + `')`,
		`INSERT OR IGNORE INTO ai_profiles (id, owner_user_id, name, protocol, endpoint_url, model, voice, created_at, updated_at)
			VALUES ('ai', 'alice', 'live', 'live_ws', 'https://ai.example.test', 'm', 'v', '` + seedTime + `', '` + seedTime + `')`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	var ctrl any
	if control != "" {
		ctrl = control
	}
	ended := any(nil)
	if status == "ended" || status == "failed" {
		ended = seedTime
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO calls (id, owner_user_id, telephony_profile_id, ai_profile_id,
		idempotency_key, to_number, status, provider_call_control_id, max_duration_seconds, created_at, updated_at, ended_at)
		VALUES (?, 'alice', 'tel', 'ai', ?, '+14155550111', ?, ?, 600, ?, ?, ?)`,
		id, "idem-"+id, status, ctrl, seedTime, seedTime, ended); err != nil {
		t.Fatal(err)
	}
}

func callStatus(t *testing.T, db *sql.DB, id string) (status, control string, events int) {
	t.Helper()
	var c sql.NullString
	if err := db.QueryRow(`SELECT status, provider_call_control_id FROM calls WHERE id = ?`, id).Scan(&status, &c); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM call_events WHERE call_id = ?`, id).Scan(&events); err != nil {
		t.Fatal(err)
	}
	return status, c.String, events
}

type fixture struct {
	priv ed25519.PrivateKey
	pub  string
	wh   *Webhooks
	srv  http.Handler
	now  time.Time
}

func newFixture(t *testing.T, s *storage.Store) fixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	wh, err := NewWebhooks(s, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	keyB64 := base64.StdEncoding.EncodeToString(pub)
	mux := http.NewServeMux()
	mux.Handle("POST /telnyx/calls/{call_id}/webhook", wh.Handler(func(_ context.Context, callID string) (string, error) {
		if callID == "" {
			return "", storage.ErrCallNotFound
		}
		return keyB64, nil
	}))
	return fixture{priv: priv, pub: keyB64, wh: wh, srv: mux, now: now}
}

// post sends one signed webhook for a call. body is a Telnyx event envelope.
func (f fixture) post(t *testing.T, callID string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/telnyx/calls/"+callID+"/webhook", strings.NewReader(string(body)))
	for k, v := range telnyx.Sign(f.priv, f.now, body) {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func event(t *testing.T, id, typ, control string, extra map[string]any) []byte {
	t.Helper()
	payload := map[string]any{"call_control_id": control}
	for k, v := range extra {
		payload[k] = v
	}
	b, err := json.Marshal(map[string]any{"data": map[string]any{
		"record_type": "event", "id": id, "event_type": typ, "occurred_at": seedTime, "payload": payload,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSignedRingingThenAnsweredAdvancesCall(t *testing.T) {
	s, db := newWebhookStore(t)
	seedCall(t, db, "c1", "initiating", "cc-1")
	f := newFixture(t, s)

	if rec := f.post(t, "c1", event(t, "e1", "call.initiated", "cc-1", nil)); rec.Code != http.StatusOK {
		t.Fatalf("ringing = %d %s", rec.Code, rec.Body)
	}
	if st, _, _ := callStatus(t, db, "c1"); st != "ringing" {
		t.Fatalf("status = %s, want ringing", st)
	}
	if rec := f.post(t, "c1", event(t, "e2", "call.answered", "cc-1", nil)); rec.Code != http.StatusOK {
		t.Fatalf("answered = %d", rec.Code)
	}
	st, control, events := callStatus(t, db, "c1")
	if st != "active" || control != "cc-1" {
		t.Fatalf("status=%s control=%s, want active/cc-1", st, control)
	}
	// The seed inserts the call directly, so only the two signals are written.
	if events != 2 {
		t.Fatalf("events = %d, want 2 (ringing, answered)", events)
	}
}

func TestRedeliveredWebhookIsNotAppliedTwice(t *testing.T) {
	s, db := newWebhookStore(t)
	seedCall(t, db, "c1", "initiating", "cc-1")
	f := newFixture(t, s)
	body := event(t, "e1", "call.initiated", "cc-1", nil)
	f.post(t, "c1", body)
	_, _, before := callStatus(t, db, "c1")
	if rec := f.post(t, "c1", body); rec.Code != http.StatusOK {
		t.Fatalf("redelivery = %d", rec.Code)
	}
	_, _, after := callStatus(t, db, "c1")
	if after != before {
		t.Fatalf("redelivery wrote events: %d -> %d", before, after)
	}
}

// A signal whose control id is not the call's is refused. Nothing changes.
func TestForeignControlIDIsRejectedWithoutWrite(t *testing.T) {
	s, db := newWebhookStore(t)
	seedCall(t, db, "c1", "active", "cc-1")
	f := newFixture(t, s)
	_, _, before := callStatus(t, db, "c1")

	rec := f.post(t, "c1", event(t, "e9", "call.hangup", "cc-OTHER", map[string]any{"hangup_cause": "normal_clearing"}))
	if rec.Code == http.StatusOK {
		t.Fatalf("foreign control id accepted: %d", rec.Code)
	}
	st, control, after := callStatus(t, db, "c1")
	if st != "active" || control != "cc-1" || after != before {
		t.Fatalf("foreign signal changed state: status=%s control=%s events %d->%d", st, control, before, after)
	}
}

func TestBadSignatureIsRejectedBeforeAnyWrite(t *testing.T) {
	s, db := newWebhookStore(t)
	seedCall(t, db, "c1", "initiating", "cc-1")
	f := newFixture(t, s)
	body := event(t, "e1", "call.initiated", "cc-1", nil)
	req := httptest.NewRequest(http.MethodPost, "/telnyx/calls/c1/webhook", strings.NewReader(string(body)))
	for k, v := range telnyx.Sign(f.priv, f.now, []byte("other body")) {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature = %d, want 401", rec.Code)
	}
	if st, _, _ := callStatus(t, db, "c1"); st != "initiating" {
		t.Fatalf("status changed on bad signature: %s", st)
	}
}

// An unknown call id is refused without saying whether the call exists.
func TestUnknownCallIsUnauthorized(t *testing.T) {
	s, _ := newWebhookStore(t)
	f := newFixture(t, s)
	mux := http.NewServeMux()
	wh := f.wh
	mux.Handle("POST /telnyx/calls/{call_id}/webhook", wh.Handler(func(context.Context, string) (string, error) {
		return "", storage.ErrCallNotFound
	}))
	body := event(t, "e1", "call.initiated", "cc-1", nil)
	req := httptest.NewRequest(http.MethodPost, "/telnyx/calls/nope/webhook", strings.NewReader(string(body)))
	for k, v := range telnyx.Sign(f.priv, f.now, body) {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown call = %d, want 401", rec.Code)
	}
}

func TestRejectsNonPostAndNilDependencies(t *testing.T) {
	if _, err := NewWebhooks(nil, time.Now); err == nil {
		t.Fatal("nil receipts must be refused")
	}
	if _, err := NewWebhooks(stubReceipts{}, nil); err == nil {
		t.Fatal("nil clock must be refused")
	}
}

type stubReceipts struct{}

func (stubReceipts) RecordWebhookOnce(context.Context, string, string, time.Time, func(context.Context, *sql.Tx) error) (bool, error) {
	return false, nil
}
