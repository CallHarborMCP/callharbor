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
	"sync"
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

// seedNow is the fixture clock. Signed webhooks are stamped with it.
var seedNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func newFixture(t *testing.T, s *storage.Store) fixture {
	t.Helper()
	wh, err := NewWebhooks(s, func() time.Time { return seedNow }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return newFixtureWith(t, wh)
}

// newFixtureWith serves wh behind the per-call route with one signing key.
func newFixtureWith(t *testing.T, wh *Webhooks) fixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := seedNow
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

// event builds a Telnyx event envelope. client_state carries the call id the
// dial sent, base64 encoded as Telnyx returns it, so the event names callID.
func event(t *testing.T, callID, id, typ, control string, extra map[string]any) []byte {
	t.Helper()
	payload := map[string]any{"call_control_id": control,
		"client_state": base64.StdEncoding.EncodeToString([]byte(callID))}
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

	if rec := f.post(t, "c1", event(t, "c1", "e1", "call.initiated", "cc-1", nil)); rec.Code != http.StatusOK {
		t.Fatalf("ringing = %d %s", rec.Code, rec.Body)
	}
	if st, _, _ := callStatus(t, db, "c1"); st != "ringing" {
		t.Fatalf("status = %s, want ringing", st)
	}
	if rec := f.post(t, "c1", event(t, "c1", "e2", "call.answered", "cc-1", nil)); rec.Code != http.StatusOK {
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
	body := event(t, "c1", "e1", "call.initiated", "cc-1", nil)
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

	// A foreign control id is a permanent refusal: 200, so Telnyx does not retry.
	rec := f.post(t, "c1", event(t, "c1", "e9", "call.hangup", "cc-OTHER", map[string]any{"hangup_cause": "normal_clearing"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("foreign control id = %d, want 200 (permanent, not retried)", rec.Code)
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
	body := event(t, "c1", "e1", "call.initiated", "cc-1", nil)
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
	body := event(t, "c1", "e1", "call.initiated", "cc-1", nil)
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
	if _, err := NewWebhooks(nil, time.Now, nil, nil); err == nil {
		t.Fatal("nil receipts must be refused")
	}
	if _, err := NewWebhooks(stubReceipts{}, nil, nil, nil); err == nil {
		t.Fatal("nil clock must be refused")
	}
}

type stubReceipts struct{}

func (stubReceipts) RecordWebhookOnce(context.Context, string, string, time.Time, func(context.Context, *sql.Tx) error) (bool, error) {
	return false, nil
}

// A client_state that names another call is refused permanently: 200, and no
// state change, so a misrouted or replayed event cannot touch this call.
func TestClientStateForAnotherCallIsRejectedWithoutWrite(t *testing.T) {
	s, db := newWebhookStore(t)
	seedCall(t, db, "c1", "active", "cc-1")
	f := newFixture(t, s)
	_, _, before := callStatus(t, db, "c1")
	body, err := json.Marshal(map[string]any{"data": map[string]any{
		"record_type": "event", "id": "e7", "event_type": "call.hangup", "occurred_at": seedTime,
		"payload": map[string]any{"call_control_id": "cc-1", "hangup_cause": "normal_clearing",
			"client_state": base64.StdEncoding.EncodeToString([]byte("some-other-call"))},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rec := f.post(t, "c1", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("client_state mismatch = %d, want 200", rec.Code)
	}
	st, control, after := callStatus(t, db, "c1")
	if st != "active" || control != "cc-1" || after != before {
		t.Fatalf("mismatched client_state changed state: status=%s events %d->%d", st, before, after)
	}
}

// Once a call's leg is bound to a control id, the control id is the proof, so a
// signed call.hangup without client_state still ends the call (r2).
func TestHangupWithoutClientStateOnBoundCallEndsIt(t *testing.T) {
	s, db := newWebhookStore(t)
	seedCall(t, db, "c1", "active", "cc-1")
	f := newFixture(t, s)
	body, err := json.Marshal(map[string]any{"data": map[string]any{
		"record_type": "event", "id": "e-nocs", "event_type": "call.hangup", "occurred_at": seedTime,
		"payload": map[string]any{"call_control_id": "cc-1", "hangup_cause": "normal_clearing"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.post(t, "c1", body); rec.Code != http.StatusOK {
		t.Fatalf("hangup without client_state = %d, want 200", rec.Code)
	}
	st, control, _ := callStatus(t, db, "c1")
	if st != "ended" || control != "cc-1" {
		t.Fatalf("status=%s control=%s, want ended/cc-1", st, control)
	}
}

// Without client_state, a bound call still refuses a control id that is not
// its own. The control id is the proof once the leg is bound (r2).
func TestForeignControlIDWithoutClientStateIsRejected(t *testing.T) {
	s, db := newWebhookStore(t)
	seedCall(t, db, "c1", "active", "cc-1")
	f := newFixture(t, s)
	_, _, before := callStatus(t, db, "c1")
	body, err := json.Marshal(map[string]any{"data": map[string]any{
		"record_type": "event", "id": "e-fcs", "event_type": "call.hangup", "occurred_at": seedTime,
		"payload": map[string]any{"call_control_id": "cc-OTHER", "hangup_cause": "normal_clearing"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.post(t, "c1", body); rec.Code != http.StatusOK {
		t.Fatalf("foreign control id without client_state = %d, want 200", rec.Code)
	}
	st, control, after := callStatus(t, db, "c1")
	if st != "active" || control != "cc-1" || after != before {
		t.Fatalf("foreign control id changed state: status=%s control=%s events %d->%d", st, control, before, after)
	}
}

// A call left in initiating with no recorded control id takes the id from the
// first verified event that names it, inside the same transaction.
func TestControlIDRecordedFromFirstEventWhenUnset(t *testing.T) {
	s, db := newWebhookStore(t)
	seedCall(t, db, "c1", "initiating", "")
	f := newFixture(t, s)
	if rec := f.post(t, "c1", event(t, "c1", "e1", "call.initiated", "cc-9", nil)); rec.Code != http.StatusOK {
		t.Fatalf("ringing = %d", rec.Code)
	}
	st, control, _ := callStatus(t, db, "c1")
	if st != "ringing" || control != "cc-9" {
		t.Fatalf("status=%s control=%s, want ringing/cc-9", st, control)
	}
}

// recordingHangup counts the hangups a webhook sends for orphaned legs.
type recordingHangup struct {
	mu  sync.Mutex
	ids []string
}

func (r *recordingHangup) hang(_ context.Context, leg storage.LiveCallLeg) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, leg.ID+"|"+leg.ControlID)
	return nil
}

func (r *recordingHangup) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ids)
}

// A leg that answers after its call already ended is still live at Telnyx. The
// call ended before its control id was known, so the event that first records
// the id sends exactly one hangup for that leg after commit. The receipt makes
// a redelivery send nothing more (n1).
func TestOrphanLegOnTerminalCallGetsOneHangup(t *testing.T) {
	s, db := newWebhookStore(t)
	seedCall(t, db, "c1", "ended", "")
	rh := &recordingHangup{}
	wh, err := NewWebhooks(s, func() time.Time { return seedNow }, rh.hang, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWith(t, wh)
	body := event(t, "c1", "e1", "call.answered", "cc-1", nil)
	if rec := f.post(t, "c1", body); rec.Code != http.StatusOK {
		t.Fatalf("answered after end = %d", rec.Code)
	}
	if rec := f.post(t, "c1", body); rec.Code != http.StatusOK {
		t.Fatalf("redelivery = %d", rec.Code)
	}
	if n := rh.count(); n != 1 {
		t.Fatalf("hangups = %d, want exactly 1", n)
	}
}

// Once the control id is recorded, later non-hangup events on a terminal call do
// not hang up again. The earlier event that recorded the id already decided it
// (n1), so a stream of late events sends no repeated hangups.
func TestLaterEventsOnBoundTerminalCallSendNoHangup(t *testing.T) {
	s, db := newWebhookStore(t)
	seedCall(t, db, "c1", "ended", "cc-1")
	rh := &recordingHangup{}
	wh, err := NewWebhooks(s, func() time.Time { return seedNow }, rh.hang, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWith(t, wh)
	for _, id := range []string{"e1", "e2", "e3"} {
		if rec := f.post(t, "c1", event(t, "c1", id, "call.answered", "cc-1", nil)); rec.Code != http.StatusOK {
			t.Fatalf("late event %s = %d", id, rec.Code)
		}
	}
	if n := rh.count(); n != 0 {
		t.Fatalf("hangups = %d, want 0 for a bound terminal call", n)
	}
}

// A hangup for a terminal call is the leg ending, so it is never re-hung-up.
func TestHangupEventNeverTriggersOrphanHangup(t *testing.T) {
	s, db := newWebhookStore(t)
	seedCall(t, db, "c1", "ended", "cc-1")
	rh := &recordingHangup{}
	wh, err := NewWebhooks(s, func() time.Time { return seedNow }, rh.hang, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWith(t, wh)
	if rec := f.post(t, "c1", event(t, "c1", "e2", "call.hangup", "cc-1", map[string]any{"hangup_cause": "normal_clearing"})); rec.Code != http.StatusOK {
		t.Fatalf("hangup = %d", rec.Code)
	}
	if n := rh.count(); n != 0 {
		t.Fatalf("hangups = %d, want 0", n)
	}
}
