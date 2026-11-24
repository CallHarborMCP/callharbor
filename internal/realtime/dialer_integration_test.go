package realtime_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/realtime/livews"
	"github.com/CallHarborMCP/callharbor/internal/realtime/realtimews"
)

// dropServer accepts one upgrade, records the request headers, sends the
// protocol's first event and then drops the TCP connection without a close.
func dropServer(t *testing.T, first string, seen *http.Header, mu *sync.Mutex) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*seen = r.Header.Clone()
		mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		_ = c.Write(ctx, websocket.MessageText, []byte(first))
		_ = c.CloseNow()
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestCustomHeadersReachUpgradeAndReservedAreRejected(t *testing.T) {
	var (
		mu   sync.Mutex
		seen http.Header
	)
	base := dropServer(t, `{"type":"session.created","session":{"id":"s"}}`, &seen, &mu)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pcmu := audio.Format{Encoding: audio.PCMU, SampleRate: 8000}

	p := realtime.Profile{
		Protocol: realtime.ProtocolRealtime,
		Endpoint: base + "/v1/realtime",
		APIKey:   "sk-test",
		Model:    "gpt-realtime-2.1",
		Voice:    "marin",
		Format:   pcmu,
		Headers:  map[string]string{"X-Tenant": "acme"},
	}
	d := realtime.Dialers{realtime.ProtocolRealtime: realtimews.Dialer}
	// The fake drops after session.created, so the handshake fails; the
	// header must still have been sent on the upgrade.
	_, _ = d.Dial(ctx, p, realtime.Requirements{Format: pcmu})
	mu.Lock()
	if seen.Get("X-Tenant") != "acme" || seen.Get("Authorization") != "Bearer sk-test" {
		mu.Unlock()
		t.Fatalf("upgrade headers %v", seen)
	}
	mu.Unlock()

	p.Headers = map[string]string{"Authorization": "Bearer hijack"}
	if _, err := d.Dial(ctx, p, realtime.Requirements{Format: pcmu}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved header must fail closed before dialing: %v", err)
	}
}

func TestProviderDropSurfacesAsDisconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pcmu := audio.Format{Encoding: audio.PCMU, SampleRate: 8000}

	t.Run("realtime handshake then drop", func(t *testing.T) {
		// Accept the handshake fully, then drop: the first Next must fail
		// as a disconnect, not a clean end.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.created","session":{"id":"s"}}`))
			_, _, _ = c.Read(r.Context())
			_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.updated","session":{"audio":{"input":{"format":{"type":"audio/pcmu"}},"output":{"format":{"type":"audio/pcmu"}}}}}`))
			_ = c.CloseNow()
		}))
		t.Cleanup(srv.Close)
		s, err := realtimews.Dialer(ctx, realtime.Profile{
			Protocol: realtime.ProtocolRealtime,
			Endpoint: "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime",
			APIKey:   "k", Model: "m", Voice: "v", Format: pcmu,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		if _, err := s.Next(ctx); !errors.Is(err, realtime.ErrDisconnected) {
			t.Fatalf("drop must be ErrDisconnected, got %v", err)
		}
	})

	t.Run("live drop after started", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			_, _, _ = c.Read(r.Context())
			_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.started","session":{"id":"live_1","model":"gpt-live-1"}}`))
			_ = c.CloseNow()
		}))
		t.Cleanup(srv.Close)
		s, err := livews.Dialer(ctx, realtime.Profile{
			Protocol: realtime.ProtocolLive,
			Endpoint: "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/live/sessions",
			APIKey:   "k", Model: "gpt-live-1", Voice: "v", Format: pcmu,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		if _, err := s.Next(ctx); !errors.Is(err, realtime.ErrDisconnected) {
			t.Fatalf("drop must be ErrDisconnected, got %v", err)
		}
	})
}

func TestBothAdaptersSelectedExplicitly(t *testing.T) {
	d := realtime.Dialers{
		realtime.ProtocolRealtime: realtimews.Dialer,
		realtime.ProtocolLive:     livews.Dialer,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pcmu := audio.Format{Encoding: audio.PCMU, SampleRate: 8000}
	// Live must never be served by the Realtime adapter: an unreachable
	// Live endpoint fails with a dial error, not a Realtime handshake.
	_, err := d.Dial(ctx, realtime.Profile{Protocol: realtime.ProtocolLive, Endpoint: "ws://127.0.0.1:1/v1/live/sessions", APIKey: "k", Model: "m", Format: pcmu}, realtime.Requirements{Format: pcmu})
	if err == nil || errors.Is(err, realtime.ErrProtocol) {
		t.Fatalf("live dial: %v", err)
	}
	if _, err := d.Dial(ctx, realtime.Profile{Protocol: realtime.ProtocolLive, Endpoint: "wss://api.example/v1/live/sessions?model=x", APIKey: "k", Model: "m", Format: pcmu}, realtime.Requirements{Format: pcmu}); err == nil {
		t.Fatal("live endpoint with a model query must fail closed")
	}
}
