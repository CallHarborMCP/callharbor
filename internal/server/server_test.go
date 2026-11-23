package server

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/CallHarborMCP/callharbor/internal/observability"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	assets := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<h1>CallHarbor test asset</h1>")},
	}
	return New(observability.NewLogger(io.Discard, slog.LevelError), assets)
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealthzAlwaysOK(t *testing.T) {
	s := newTestServer(t)
	rec := get(t, s.Handler(), "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != `{"status":"ok"}` {
		t.Errorf("body = %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestReadyzFollowsReadinessState(t *testing.T) {
	s := newTestServer(t)

	rec := get(t, s.Handler(), "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before SetReady: status = %d, want 503", rec.Code)
	}

	s.SetReady(true)
	rec = get(t, s.Handler(), "/readyz")
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"ready"}` {
		t.Fatalf("after SetReady(true): status = %d body = %q", rec.Code, rec.Body.String())
	}

	s.SetReady(false)
	rec = get(t, s.Handler(), "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("after SetReady(false): status = %d, want 503", rec.Code)
	}
}

func TestEmbeddedAssetsAreServed(t *testing.T) {
	s := newTestServer(t)
	rec := get(t, s.Handler(), "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "CallHarbor test asset") {
		t.Errorf("body does not contain embedded asset: %q", rec.Body.String())
	}
}

func TestMissingAssetReturnsNotFound(t *testing.T) {
	s := newTestServer(t)
	if rec := get(t, s.Handler(), "/no-such-file.js"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	s := newTestServer(t)
	for _, path := range []string{"/healthz", "/readyz", "/"} {
		rec := get(t, s.Handler(), path)
		h := rec.Header()
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing nosniff", path)
		}
		if h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q", path, h.Get("X-Frame-Options"))
		}
		if h.Get("Content-Security-Policy") != contentSecurityPolicy {
			t.Errorf("%s: CSP = %q", path, h.Get("Content-Security-Policy"))
		}
		if h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: Referrer-Policy = %q", path, h.Get("Referrer-Policy"))
		}
	}
}

func TestRoutesRejectUnsupportedMethods(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", bytes.NewReader(nil)))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz status = %d, want 405", rec.Code)
	}
}
