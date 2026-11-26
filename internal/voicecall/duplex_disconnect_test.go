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
	s, errs, ctx := dxStart(t, func(ctx context.Context) (realtime.Session, error) {
		return realtimews.Connect(ctx, realtimews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-realtime-2.1", Voice: "marin", Instructions: "Test.", Format: dxLeg})
	}, realtime.RealtimeCapabilities, clk, 0)
	_ = s

	select {
	case err := <-errs:
		if !errors.Is(err, realtime.ErrDisconnected) {
			t.Fatalf("run ended with %v, want realtime.ErrDisconnected", err)
		}
	case <-ctx.Done():
		t.Fatal("run did not end after provider drop")
	}
}
