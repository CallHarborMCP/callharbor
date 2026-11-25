package observability

import (
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"
)

const redacted = "[REDACTED]"

var sensitiveKey = regexp.MustCompile(`(?i)(secret|token|password|passwd|credential|authorization|cookie|api[_-]?key|private[_-]?key|session)`)

func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: redact,
	}))
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		return a
	}
	if sensitiveKey.MatchString(a.Key) {
		return slog.String(a.Key, redacted)
	}
	return a
}

func AccessLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.InfoContext(r.Context(), "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying writer. WebSocket upgrades (coder/websocket)
// and http.ResponseController find the http.Hijacker through it. Without this,
// every upgrade behind the access log fails with 501.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
