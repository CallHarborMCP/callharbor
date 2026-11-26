package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
)

// TestWebSocketUpgradeThroughServer proves a WebSocket handler mounted behind
// the access log and security headers can complete the upgrade. The telephony
// media and stream endpoints depend on this.
func TestWebSocketUpgradeThroughServer(t *testing.T) {
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}}
	srv := New(slog.New(slog.DiscardHandler), assets, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Errorf("accept: %v", err)
				return
			}
			_ = c.CloseNow()
		})
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, strings.Replace(ts.URL, "http", "ws", 1)+"/ws", nil)
	if err != nil {
		t.Fatalf("websocket dial through server: %v", err)
	}
	_ = c.CloseNow()
}
