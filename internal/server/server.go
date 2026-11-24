package server

import (
	"io/fs"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/CallHarborMCP/callharbor/internal/observability"
)

const (
	contentSecurityPolicy = "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; object-src 'none'"
	jsonContentType       = "application/json; charset=utf-8"
)

type Server struct {
	handler http.Handler
	ready   atomic.Bool
}

func New(logger *slog.Logger, assets fs.FS, routes ...func(*http.ServeMux)) *Server {
	s := &Server{}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	for _, register := range routes {
		register(mux)
	}
	mux.Handle("GET /", staticHandler(assets))

	s.handler = observability.AccessLog(logger, securityHeaders(mux))
	return s
}

func staticHandler(assets fs.FS) http.Handler {
	files := http.FileServerFS(assets)
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusServiceUnavailable, `{"error":"web UI not built"}`)
		})
	}
	return files
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) SetReady(ready bool) {
	s.ready.Store(ready)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, `{"status":"ok"}`)
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, `{"status":"unavailable"}`)
		return
	}
	writeJSON(w, http.StatusOK, `{"status":"ready"}`)
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", jsonContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
