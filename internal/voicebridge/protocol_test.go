package voicebridge_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
)

var g711 = audio.Format{Encoding: audio.PCMU, SampleRate: 8000}

type fakeProvider struct {
	mu       sync.Mutex
	got      []map[string]any
	onStart  func(m map[string]any) []string
	afterUpd []string
	firstMsg string
}

func (f *fakeProvider) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		ctx := r.Context()
		if f.firstMsg != "" {
			_ = c.Write(ctx, websocket.MessageText, []byte(f.firstMsg))
		}
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Errorf("provider got invalid JSON: %v", err)
				return
			}
			f.mu.Lock()
			f.got = append(f.got, m)
			f.mu.Unlock()
			var out []string
			switch m["type"] {
			case "session.start":
				out = f.onStart(m)
			case "session.update":
				out = append([]string{`{"type":"session.updated","event_id":"event_upd","session":` + mustJSON(m["session"]) + `}`}, f.afterUpd...)
			case "session.close":
				out = []string{`{"type":"session.closed","event_id":"evt_closed","reason":"close_requested","usage":{"seconds":3}}`}
			}
			for _, ev := range out {
				if err := c.Write(ctx, websocket.MessageText, []byte(ev)); err != nil {
					return
				}
			}
		}
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func serve(t *testing.T, f *fakeProvider) string {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func pcmuChunk() (raw []byte, b64 string) {
	raw = make([]byte, 160)
	for i := range raw {
		raw[i] = audio.LinearToUlaw(int16(i * 80))
	}
	return raw, base64.StdEncoding.EncodeToString(raw)
}

// TestLiveAndRealtimeBehindOneBridge runs the same bridge against both
// independent protocol simulators and asserts the shared audio contract:
// binary G.711 agent audio is accounted in the playout ledger, and a barge-in
// cut discards unplayed audio.
func TestLiveAndRealtimeBehindOneBridge(t *testing.T) {
	_, b64 := pcmuChunk()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("gpt-live", func(t *testing.T) {
		f := &fakeProvider{onStart: func(m map[string]any) []string {
			s, _ := m["session"].(map[string]any)
			s["id"], s["status"] = "live_test", "active"
			return []string{
				`{"type":"session.started","event_id":"evt_started","client_event_id":"ch_session_start","session":` + mustJSON(s) + `}`,
				`{"type":"session.output_audio.delta","delta":"` + b64 + `"}`,
			}
		}}
		s, err := livews.Connect(ctx, livews.Config{Endpoint: serve(t, f) + "/v1/live/sessions", APIKey: "sk-test", Model: "gpt-live-1.1", Voice: "marin", Instructions: "Book a table.", Format: g711})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _, _ = s.CloseGracefully(ctx) }()
		runBridgeContract(t, ctx, s, g711)
	})

	t.Run("realtime", func(t *testing.T) {
		_, b64 := pcmuChunk()
		f := &fakeProvider{
			firstMsg: `{"type":"session.created","event_id":"e0","session":{"id":"sess_x"}}`,
			afterUpd: []string{`{"type":"response.output_audio.delta","event_id":"e1","response_id":"resp_1","item_id":"item_1","output_index":0,"content_index":0,"delta":"` + b64 + `"}`},
		}
		s, err := realtimews.Connect(ctx, realtimews.Config{Endpoint: serve(t, f) + "/v1/realtime", APIKey: "sk-test", Model: "gpt-realtime-2.1", Voice: "marin", Instructions: "Book a table.", Format: g711})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		runBridgeContract(t, ctx, s, g711)
	})
}

type nexter interface {
	Next(context.Context) (realtime.Event, error)
}

func runBridgeContract(t *testing.T, ctx context.Context, s nexter, leg audio.Format) {
	t.Helper()
	b, err := voicebridge.New(leg, leg, 5*time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != realtime.EventAudio {
		t.Fatalf("want agent audio, got %+v", ev)
	}
	src := audio.SourceRef{ItemID: ev.ItemID, ContentIndex: ev.ContentIndex}
	payload, mark, err := b.AgentAudio(src, ev.Audio, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != len(ev.Audio) {
		t.Fatalf("G.711 passthrough length %d, want %d", len(payload), len(ev.Audio))
	}
	if mark == "" {
		t.Fatal("agent audio must carry a playout mark")
	}
	b.Interrupt(time.Now())
	it, ok := b.Item(src)
	if !ok || !it.Interrupted {
		t.Fatalf("barge-in cut not recorded: %+v ok=%v", it, ok)
	}
}
