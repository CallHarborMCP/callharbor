package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoggerRedactsSensitiveAttributes(t *testing.T) {
	const secretValue = "sk-live-super-secret-value"
	var buf bytes.Buffer
	logger := NewLogger(&buf, slog.LevelDebug)

	logger.Info("provider configured",
		"api_key", secretValue,
		"Authorization", "Bearer "+secretValue,
		"session_id", secretValue,
		"refresh_token", secretValue,
		"db_password", secretValue,
		"client_secret", secretValue,
		"cookie", secretValue,
		"private_key", secretValue,
		"provider", "telnyx",
	)

	out := buf.String()
	if strings.Contains(out, secretValue) {
		t.Fatalf("log output leaked secret: %s", out)
	}

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("log line is not JSON: %v: %s", err, out)
	}
	if record["api_key"] != redacted {
		t.Errorf("api_key = %v, want %s", record["api_key"], redacted)
	}
	if record["provider"] != "telnyx" {
		t.Errorf("provider = %v, want telnyx (non-sensitive values must survive)", record["provider"])
	}
}

func TestLoggerEmitsStructuredJSONAtConfiguredLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, slog.LevelWarn)

	logger.Info("suppressed")
	logger.Warn("kept", "component", "server")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), buf.String())
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if record["msg"] != "kept" || record["level"] != "WARN" || record["component"] != "server" {
		t.Errorf("unexpected record: %v", record)
	}
}

func TestAccessLogKeepsFirstStatusWrittenBeforeBody(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, slog.LevelInfo)
	handler := AccessLog(logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("body"))
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &record); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if record["status"] != float64(http.StatusOK) {
		t.Errorf("status = %v, want 200 implicit from body write", record["status"])
	}
	if rec.Body.String() != "body" {
		t.Errorf("body = %q, want body", rec.Body.String())
	}
}

func TestAccessLogRecordsStatusWithoutQueryString(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, slog.LevelInfo)
	handler := AccessLog(logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodGet, "/some/path?token=supersecret", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if strings.Contains(out, "supersecret") {
		t.Fatalf("access log leaked query string: %s", out)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &record); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if record["status"] != float64(http.StatusTeapot) || record["path"] != "/some/path" {
		t.Errorf("unexpected access record: %v", record)
	}
}
