package secrets

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CallHarborMCP/callharbor/internal/identity"
	"github.com/CallHarborMCP/callharbor/internal/observability"
)

// TestKeyMaterialNeverReachesLogs runs the secret API through the production
// access logger and captures everything it writes. The provider key and the
// master key must not appear in the output, while the request is still logged.
func TestKeyMaterialNeverReachesLogs(t *testing.T) {
	masterKey := testKey(42)
	masterB64 := base64.StdEncoding.EncodeToString(masterKey)
	const providerKey = "sk-live-LOGCHECK-55019"

	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelDebug)

	f := newAPIFixture(t, true)
	handler := observability.AccessLog(logger, f.mux)

	cookie, csrf := f.login(t, "root", pwAdmin)
	body := strings.NewReader(`{"name":"openai","purpose":"ai_api_key","value":"` + providerKey + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/secrets", body)
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.30:5555"
	req.AddCookie(cookie)
	req.Header.Set(identity.CSRFHeader, csrf)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}

	out := buf.String()
	for _, secret := range []string{providerKey, masterB64} {
		if strings.Contains(out, secret) {
			t.Fatalf("log output contains secret material %q", secret)
		}
	}
	if !strings.Contains(out, "http request") {
		t.Fatalf("access log did not record the request: %q", out)
	}
}
