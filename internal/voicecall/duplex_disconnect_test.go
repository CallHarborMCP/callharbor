package voicecall_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/realtime/livews"
	"github.com/CallHarborMCP/callharbor/internal/realtime/realtimews"
)

// TestDuplexProviderDropEndsRunAsDisconnect checks that a provider connection
// drop mid-call ends the production run with realtime.ErrDisconnected and is
// never reported as a clean close. The provider sends one audio chunk, then
// closes the socket without session.closed.
func TestDuplexProviderDropEndsRunAsDisconnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created","event_id":"evt_c","session":{"id":"sess_drop","type":"realtime"}}`))
		for {
			_, raw, err := c.Read(ctx)
			if err != nil {
				return
			}
			if strings.Contains(string(raw), `"session.update"`) {
				// Echo the format the client asked for, then drop the socket.
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.updated","event_id":"evt_u","session":{"id":"sess_drop","type":"realtime","audio":{"input":{"format":{"type":"audio/pcmu"}},"output":{"format":{"type":"audio/pcmu"}}}}}`))
				_ = c.CloseNow()
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	endpoint := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"

	clk := newDxClock()
	s, errs, ctx, stop := dxStart(t, func(ctx context.Context) (realtime.Session, error) {
		return realtimews.Connect(ctx, realtimews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-realtime-2.1", Voice: "marin", Instructions: "Test.", Format: dxLeg})
	}, realtime.RealtimeCapabilities, clk, 0)
	select {
	case err := <-errs:
		if !errors.Is(err, realtime.ErrDisconnected) {
			t.Fatalf("run ended with %v, want realtime.ErrDisconnected", err)
		}
	case <-ctx.Done():
		t.Fatal("run did not end after provider drop")
	}
	// The run has already returned; only the leg needs closing.
	_ = s.Close()
	stop()
}

// TestDuplexLiveProviderDropEndsRunAsDisconnect is the live_ws counterpart of
// TestDuplexProviderDropEndsRunAsDisconnect: the provider drops the socket
// without session.closed, and Run must end with realtime.ErrDisconnected.
func TestDuplexLiveProviderDropEndsRunAsDisconnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		for {
			_, raw, err := c.Read(ctx)
			if err != nil {
				return
			}
			if strings.Contains(string(raw), `"session.start"`) {
				// Confirm the session, then drop the socket with no session.closed.
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.started","event_id":"evt_started","client_event_id":"ch_session_start","session":{"id":"live_drop","status":"active"}}`))
				_ = c.CloseNow()
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	endpoint := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/live/sessions"

	clk := newDxClock()
	s, errs, ctx, stop := dxStart(t, func(ctx context.Context) (realtime.Session, error) {
		return livews.Connect(ctx, livews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-live-1.1", Voice: "marin", Instructions: "Test.", Format: dxLeg})
	}, realtime.LiveCapabilities, clk, 0)

	select {
	case err := <-errs:
		if !errors.Is(err, realtime.ErrDisconnected) {
			t.Fatalf("run ended with %v, want realtime.ErrDisconnected", err)
		}
	case <-ctx.Done():
		t.Fatal("run did not end after the Live provider dropped")
	}
	// The run has already returned; only the leg needs closing.
	_ = s.Close()
	stop()
}
