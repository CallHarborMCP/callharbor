package telnyx

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/call"
	"github.com/CallHarborMCP/callharbor/internal/storage"
)

type memReceipts struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (m *memReceipts) RecordWebhookOnce(ctx context.Context, provider, eventID string, _ time.Time, process func(context.Context, *sql.Tx) error) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := provider + "|" + eventID
	if m.seen[key] {
		return true, nil
	}
	if err := process(ctx, nil); err != nil {
		return false, err
	}
	m.seen[key] = true
	return false, nil
}

type ingressHarness struct {
	priv     ed25519.PrivateKey
	now      time.Time
	receipts *memReceipts
	signals  []call.Signal
	fail     error
	ingress  *WebhookIngress
}

func newIngressHarness(t *testing.T) *ingressHarness {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h := &ingressHarness{priv: priv, now: time.Unix(1700000000, 0), receipts: &memReceipts{seen: map[string]bool{}}}
	v, err := NewVerifier(pub, DefaultTolerance, func() time.Time { return h.now })
	if err != nil {
		t.Fatal(err)
	}
	h.ingress, err = NewWebhookIngress(v, h.receipts, func(_ context.Context, _ *sql.Tx, _ WebhookEvent, sig call.Signal) error {
		if h.fail != nil {
			return h.fail
		}
		h.signals = append(h.signals, sig)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *ingressHarness) post(t *testing.T, body []byte, hdr http.Header) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telnyx", bytes.NewReader(body))
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	rec := httptest.NewRecorder()
	h.ingress.ServeHTTP(rec, req)
	return rec.Code
}

func TestIngressAcceptsSignedWebhookOnce(t *testing.T) {
	h := newIngressHarness(t)
	body := loadWebhook(t, "callAnswered")
	if code := h.post(t, body, Sign(h.priv, h.now, body)); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(h.signals) != 1 || h.signals[0].Kind != call.KindAnswered {
		t.Fatalf("signals %+v", h.signals)
	}
	if code := h.post(t, body, Sign(h.priv, h.now.Add(10*time.Second), body)); code != http.StatusOK {
		t.Fatalf("redelivery status %d", code)
	}
	if len(h.signals) != 1 {
		t.Fatalf("redelivery dispatched again: %d signals", len(h.signals))
	}
}

func TestIngressRejectsUnsignedTamperedAndStaleWithoutStateChange(t *testing.T) {
	body := loadWebhook(t, "callAnswered")
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)/2] ^= 0x01

	cases := []struct {
		name string
		body []byte
		hdr  func(h *ingressHarness) http.Header
		want int
	}{
		{"unsigned", body, func(*ingressHarness) http.Header { return http.Header{} }, http.StatusUnauthorized},
		{"tampered", tampered, func(h *ingressHarness) http.Header { return Sign(h.priv, h.now, body) }, http.StatusUnauthorized},
		{"stale", body, func(h *ingressHarness) http.Header { return Sign(h.priv, h.now.Add(-time.Hour), body) }, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newIngressHarness(t)
			if code := h.post(t, tc.body, tc.hdr(h)); code != tc.want {
				t.Fatalf("status %d, want %d", code, tc.want)
			}
			if len(h.signals) != 0 || len(h.receipts.seen) != 0 {
				t.Fatalf("rejected request changed state: signals=%d receipts=%d", len(h.signals), len(h.receipts.seen))
			}
		})
	}
}

func TestIngressRejectsMalformedSignedBody(t *testing.T) {
	h := newIngressHarness(t)
	body := []byte(`{"data":{"record_type":"event"}}`)
	if code := h.post(t, body, Sign(h.priv, h.now, body)); code != http.StatusBadRequest {
		t.Fatalf("status %d", code)
	}
	if len(h.receipts.seen) != 0 {
		t.Fatal("malformed body recorded a receipt")
	}
}

func TestIngressRejectsOversizedBody(t *testing.T) {
	h := newIngressHarness(t)
	body := bytes.Repeat([]byte("a"), MaxWebhookBody+1)
	if code := h.post(t, body, Sign(h.priv, h.now, body)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", code)
	}
}

func TestIngressRejectsNonPOST(t *testing.T) {
	h := newIngressHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/webhooks/telnyx", nil)
	rec := httptest.NewRecorder()
	h.ingress.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestIngressFailedDispatchReturnsRetryableErrorAndNoReceipt(t *testing.T) {
	h := newIngressHarness(t)
	h.fail = errors.New("store down")
	body := loadWebhook(t, "callAnswered")
	if code := h.post(t, body, Sign(h.priv, h.now, body)); code != http.StatusInternalServerError {
		t.Fatalf("status %d", code)
	}
	if len(h.receipts.seen) != 0 {
		t.Fatal("failed dispatch recorded a receipt")
	}
	h.fail = nil
	if code := h.post(t, body, Sign(h.priv, h.now.Add(time.Second), body)); code != http.StatusOK {
		t.Fatalf("retry status %d", code)
	}
	if len(h.signals) != 1 {
		t.Fatalf("retry not dispatched: %d", len(h.signals))
	}
}

func TestIngressAcksUnmappedEventWithoutReceipt(t *testing.T) {
	h := newIngressHarness(t)
	body := []byte(`{"data":{"event_type":"call.speak.started","id":"x-1","record_type":"event","payload":{"call_control_id":"v3:a"}}}`)
	if code := h.post(t, body, Sign(h.priv, h.now, body)); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(h.receipts.seen) != 0 || len(h.signals) != 0 {
		t.Fatal("unmapped event changed state")
	}
}

func TestIngressRedeliveryAfterStoreReopenIsNoop(t *testing.T) {
	ctx := context.Background()
	h := newIngressHarness(t)
	path := filepath.Join(t.TempDir(), "app.db")
	body := loadWebhook(t, "callAnswered")

	open := func() (*storage.Store, *WebhookIngress, *int) {
		s, err := storage.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		dispatched := 0
		wi, err := NewWebhookIngress(h.ingress.verifier, s, func(_ context.Context, tx *sql.Tx, _ WebhookEvent, sig call.Signal) error {
			dispatched++
			_, err := tx.ExecContext(ctx, `INSERT INTO retention_purge_markers (scope, key, cutoff_at, created_at)
				VALUES ('call', ?, NULL, '2026-11-23T10:00:00.000Z')`, sig.EventID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return s, wi, &dispatched
	}

	s, wi, dispatched := open()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/telnyx", bytes.NewReader(body))
	for k, vs := range Sign(h.priv, h.now, body) {
		req.Header[k] = vs
	}
	rec := httptest.NewRecorder()
	wi.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || *dispatched != 1 {
		t.Fatalf("first delivery: status %d dispatched %d", rec.Code, *dispatched)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, wi2, dispatched2 := open()
	defer func() { _ = s2.Close() }()
	req = httptest.NewRequest(http.MethodPost, "/webhooks/telnyx", bytes.NewReader(body))
	for k, vs := range Sign(h.priv, h.now.Add(30*time.Second), body) {
		req.Header[k] = vs
	}
	rec = httptest.NewRecorder()
	wi2.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("redelivery status %d", rec.Code)
	}
	if *dispatched2 != 0 {
		t.Fatalf("redelivery after reopen dispatched %d times", *dispatched2)
	}
}

func TestIngressDispatchWritesCommitWithReceiptOnSQLite(t *testing.T) {
	ctx := context.Background()
	h := newIngressHarness(t)
	path := filepath.Join(t.TempDir(), "app.db")
	s, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	countState := func() int {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM retention_purge_markers").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	fail := false
	wi, err := NewWebhookIngress(h.ingress.verifier, s, func(ctx context.Context, tx *sql.Tx, _ WebhookEvent, sig call.Signal) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO retention_purge_markers (scope, key, cutoff_at, created_at)
			VALUES ('call', ?, NULL, '2026-11-23T10:00:00.000Z')`, sig.EventID); err != nil {
			return err
		}
		if fail {
			return errors.New("boom")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	body := loadWebhook(t, "callAnswered")
	post := func(at time.Time) int {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/telnyx", bytes.NewReader(body))
		for k, vs := range Sign(h.priv, at, body) {
			req.Header[k] = vs
		}
		rec := httptest.NewRecorder()
		wi.ServeHTTP(rec, req)
		return rec.Code
	}

	fail = true
	if code := post(h.now); code != http.StatusInternalServerError {
		t.Fatalf("failed dispatch status %d", code)
	}
	if n := countState(); n != 0 {
		t.Fatalf("failed dispatch left %d state rows", n)
	}

	fail = false
	if code := post(h.now.Add(time.Second)); code != http.StatusOK {
		t.Fatalf("retry status %d", code)
	}
	if code := post(h.now.Add(2 * time.Second)); code != http.StatusOK {
		t.Fatalf("redelivery status %d", code)
	}
	if n := countState(); n != 1 {
		t.Fatalf("state rows after retry and redelivery: %d, want 1", n)
	}
}

func TestNewWebhookIngressRequiresDependencies(t *testing.T) {
	if _, err := NewWebhookIngress(nil, &memReceipts{}, func(context.Context, *sql.Tx, WebhookEvent, call.Signal) error { return nil }); err == nil {
		t.Fatal("accepted nil verifier")
	}
}
