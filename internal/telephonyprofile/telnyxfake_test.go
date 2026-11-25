package telephonyprofile

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// telnyxREST is an independent fake of the Telnyx REST endpoints that
// validation reads. It is deliberately separate from the telnyx client and from
// the telnyxfake package, so a client bug cannot hide behind a matching fake.
// It serves GET only and records every request, so tests can prove that
// validation never dials.
type telnyxREST struct {
	mu sync.Mutex

	validKey string // the only accepted bearer key
	apps     map[string]appFixture
	numbers  map[string]numberFixture // keyed by E.164

	requests []recordedReq
}

type appFixture struct {
	active bool
}

type numberFixture struct {
	id           string
	status       string
	connectionID string
}

type recordedReq struct {
	method string
	path   string
	query  string
}

func newTelnyxREST(validKey string) *telnyxREST {
	return &telnyxREST{
		validKey: validKey,
		apps:     map[string]appFixture{},
		numbers:  map[string]numberFixture{},
	}
}

func (f *telnyxREST) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, recordedReq{method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery})

	if r.Method != http.MethodGet {
		writeFake(w, http.StatusMethodNotAllowed, `{"errors":[{"detail":"read-only fake"}]}`)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.validKey {
		writeFake(w, http.StatusUnauthorized, `{"errors":[{"detail":"invalid api key"}]}`)
		return
	}
	switch {
	case strings.HasPrefix(r.URL.EscapedPath(), "/v2/call_control_applications/"):
		id := strings.TrimPrefix(r.URL.EscapedPath(), "/v2/call_control_applications/")
		app, ok := f.apps[id]
		if !ok {
			writeFake(w, http.StatusNotFound, `{"errors":[{"detail":"not found"}]}`)
			return
		}
		writeFake(w, http.StatusOK, `{"data":{"id":"`+id+`","application_name":"app","active":`+boolJSON(app.active)+`}}`)
	case r.URL.EscapedPath() == "/v2/phone_numbers":
		// The contract returns no results for non-numeric filter characters and
		// needs at least three digits. Digits-only filters match a stored E.164
		// number with its leading "+" removed.
		digits := r.URL.Query().Get("filter[phone_number]")
		if len(digits) < 3 || !digitsOnly(digits) {
			writeFake(w, http.StatusOK, `{"data":[],"meta":{"total_results":0}}`)
			return
		}
		matched := []map[string]any{} // an empty JSON array, never null
		for e164, n := range f.numbers {
			if strings.TrimPrefix(e164, "+") != digits {
				continue
			}
			matched = append(matched, map[string]any{
				"id": n.id, "phone_number": e164, "status": n.status,
				"connection_id": n.connectionID, "record_type": "phone_number",
			})
		}
		body, _ := json.Marshal(map[string]any{"data": matched, "meta": map[string]any{"total_results": len(matched)}})
		writeFake(w, http.StatusOK, string(body))
	default:
		writeFake(w, http.StatusNotFound, `{"errors":[{"detail":"unknown path"}]}`)
	}
}

func (f *telnyxREST) recorded() []recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedReq(nil), f.requests...)
}

// digitsOnly reports whether s is non-empty and all ASCII digits.
func digitsOnly(s string) bool {
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

func writeFake(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func newFakeServer(t *testing.T, f *telnyxREST) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv
}
