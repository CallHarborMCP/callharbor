package livews

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/recording"
)

func upstream(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "upstream", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUpstreamServerEventsParse(t *testing.T) {
	ev, err := ParseServerEvent(upstream(t, "session.input_transcript.delta"))
	if err != nil || ev.Kind != realtime.EventCallerTranscriptDelta || ev.Text != "A table for two at seven, please." || !ev.HasTiming || ev.StartMS != 1600 || ev.EndMS != 3400 || ev.EventID != "evt_input_transcript_001" {
		t.Fatalf("%+v %v", ev, err)
	}
	ev, err = ParseServerEvent(upstream(t, "session.output_transcript.delta"))
	if err != nil || ev.Kind != realtime.EventAgentTranscriptDelta || ev.StartMS != 5400 || ev.EndMS != 7200 {
		t.Fatalf("%+v %v", ev, err)
	}
	ev, err = ParseServerEvent(upstream(t, "session.output_audio.delta"))
	if err != nil || ev.Kind != realtime.EventAudio || len(ev.Audio) != 16 {
		t.Fatalf("%+v %v", ev, err)
	}
	ev, err = ParseServerEvent(upstream(t, "session.usage.updated"))
	if err != nil || ev.Kind != realtime.EventUsage || ev.UsageSeconds != 32 {
		t.Fatalf("%+v %v", ev, err)
	}
	ev, err = ParseServerEvent(upstream(t, "session.delegation.created"))
	if err != nil || ev.Kind != realtime.EventDelegation || ev.DelegationID != "del_abc123" {
		t.Fatalf("%+v %v", ev, err)
	}
	ev, err = ParseServerEvent(upstream(t, "session.closed"))
	if err != nil || ev.Kind != realtime.EventClosed || ev.CloseReason != "close_requested" || ev.UsageSeconds != 45 {
		t.Fatalf("%+v %v", ev, err)
	}
	ev, err = ParseServerEvent(upstream(t, "error"))
	if err != nil || ev.Kind != realtime.EventError || ev.ErrCode != "unknown_parameter" {
		t.Fatalf("%+v %v", ev, err)
	}
	ev, err = ParseServerEvent([]byte(`{"type":"error","event_id":"e","error":{"type":"server_error","code":null,"message":"x"}}`))
	if err != nil || ev.ErrCode != "" {
		t.Fatalf("null error code (documented in live-conversations) must parse: %+v %v", ev, err)
	}
	ev, err = ParseServerEvent([]byte(`{"type":"future.live.event","marker":42}`))
	if err != nil || ev.Kind != realtime.EventIgnored {
		t.Fatalf("unknown future events must be tolerated: %+v %v", ev, err)
	}
	ev, err = ParseServerEvent([]byte(`{"type":"session.usage.updated","event_id":"u","usage":{"seconds":32.5}}`))
	if err != nil || ev.UsageSeconds != 32.5 {
		t.Fatalf("fractional seconds (python SDK type) must parse: %+v %v", ev, err)
	}
}

func TestSessionStartMatchesUpstreamShape(t *testing.T) {
	b, err := SessionStart(Config{Model: "gpt-live-1.1", Voice: "marin", Instructions: "Help the caller plan a restaurant reservation. Confirm details before booking.", Format: audio.Format{Encoding: audio.L16, SampleRate: 24000, LittleEndian: true}})
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	_ = json.Unmarshal(b, &got)
	_ = json.Unmarshal(upstream(t, "session.start"), &want)
	delete(got, "event_id")
	delete(want, "event_id")
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("session.start\n got %s\nwant %s", g, w)
	}
	b, _ = SessionStart(Config{Model: "m", Format: audio.Format{Encoding: audio.PCMU, SampleRate: 8000}})
	if !strings.Contains(string(b), `"format":{"rate":8000,"type":"audio/pcmu"}`) {
		t.Fatalf("pcmu %s", b)
	}
	if _, err := SessionStart(Config{Format: audio.Format{Encoding: audio.PCMU, SampleRate: 8000}}); err == nil {
		t.Fatal("missing model accepted")
	}
	for _, f := range []audio.Format{{Encoding: audio.L16, SampleRate: 8000, LittleEndian: true}, {Encoding: audio.L16, SampleRate: 24000}, {Encoding: "OPUS", SampleRate: 48000}} {
		if _, err := SessionStart(Config{Model: "m", Format: f}); !errors.Is(err, realtime.ErrCapability) {
			t.Errorf("%v accepted: %v", f, err)
		}
	}
}

func TestServerEventsNegative(t *testing.T) {
	cases := map[string]string{
		"no type":              `{"event_id":"x"}`,
		"audio not base64":     `{"type":"session.output_audio.delta","delta":"**"}`,
		"audio missing":        `{"type":"session.output_audio.delta"}`,
		"transcript no timing": `{"type":"session.input_transcript.delta","event_id":"e","delta":"hi"}`,
		"transcript no id":     `{"type":"session.output_transcript.delta","delta":"hi","start_ms":1,"end_ms":2}`,
		"reversed timing":      `{"type":"session.input_transcript.delta","event_id":"e","delta":"hi","start_ms":5,"end_ms":2}`,
		"usage missing":        `{"type":"session.usage.updated","event_id":"e"}`,
		"delegation no id":     `{"type":"session.delegation.created","event_id":"e","delegation":{}}`,
		"realtime audio":       `{"type":"response.output_audio.delta","response_id":"r","item_id":"i","delta":"AAA="}`,
		"realtime created":     `{"type":"session.created","session":{"id":"s"}}`,
		"realtime truncated":   `{"type":"conversation.item.truncated","item_id":"i","audio_end_ms":1}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseServerEvent([]byte(raw)); !errors.Is(err, realtime.ErrProtocol) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// fakeLive is an independent emulator of the documented GPT-Live primary
// WebSocket startup and close: client-first session.start, no audio accepted
// before session.started, session.close answered with session.closed.
type fakeLive struct {
	mu         sync.Mutex
	got        []map[string]any
	path       string
	query      string
	started    func(start map[string]any) []byte
	afterStart []string
}

func (f *fakeLive) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.path, f.query = r.URL.Path, r.URL.RawQuery
		f.mu.Unlock()
		if r.URL.RawQuery != "" {
			http.Error(w, "no query parameters", http.StatusBadRequest)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		ctx := r.Context()
		started := false
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(b, &m) != nil {
				return
			}
			f.mu.Lock()
			f.got = append(f.got, m)
			f.mu.Unlock()
			switch m["type"] {
			case "session.start":
				s, _ := m["session"].(map[string]any)
				if started || s == nil || s["model"] == nil {
					_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"error","event_id":"e","error":{"type":"invalid_request_error","code":"invalid_session_start","message":"bad start"}}`))
					continue
				}
				started = true
				var out []byte
				if f.started != nil {
					out = f.started(m)
				} else {
					s["id"], s["status"] = "live_test", "active"
					out, _ = json.Marshal(map[string]any{"type": "session.started", "event_id": "evt_started", "client_event_id": m["event_id"], "session": s})
				}
				_ = c.Write(ctx, websocket.MessageText, out)
				for _, ev := range f.afterStart {
					_ = c.Write(ctx, websocket.MessageText, []byte(ev))
				}
			case "session.input_audio.append":
				if !started {
					_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"error","event_id":"e","error":{"type":"invalid_request_error","code":"session_not_started","message":"audio before start"}}`))
				}
			case "session.commentary.append":
				b, _ := json.Marshal(map[string]any{"type": "session.commentary.appended", "event_id": "evt_c", "client_event_id": m["event_id"], "start_ms": 1, "end_ms": 2})
				_ = c.Write(ctx, websocket.MessageText, b)
			case "session.close":
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.closed","event_id":"evt_closed","reason":"close_requested","usage":{"seconds":12}}`))
			}
		}
	}
}

func startFake(t *testing.T, f *fakeLive) string {
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/live/sessions"
}

func cfg(ep string) Config {
	return Config{Endpoint: ep, APIKey: "sk-test", Model: "gpt-live-1.1", Voice: "marin", Instructions: "Book a table.", Format: audio.Format{Encoding: audio.PCMU, SampleRate: 8000}}
}

func TestSessionLifecycleAndTwoPartyTranscript(t *testing.T) {
	pcmu := base64.StdEncoding.EncodeToString(make([]byte, 160))
	f := &fakeLive{afterStart: []string{
		`{"type":"session.output_audio.delta","delta":"` + pcmu + `"}`,
		`{"type":"session.output_transcript.delta","event_id":"o1","delta":"Hello, this is the reservation assistant.","start_ms":0,"end_ms":2100}`,
		`{"type":"session.input_transcript.delta","event_id":"i1","delta":"Hi, I need a table.","start_ms":1800,"end_ms":3000}`,
		`{"type":"session.delegation.created","event_id":"d1","offset_ms":3100,"delegation":{"id":"del_1","type":"delegation","target":"client"}}`,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Connect(ctx, cfg(startFake(t, f)))
	if err != nil {
		t.Fatal(err)
	}
	if f.path != "/v1/live/sessions" || f.query != "" || s.SessionID != "live_test" {
		t.Fatalf("path=%q query=%q id=%q", f.path, f.query, s.SessionID)
	}
	if err := s.AppendAudio(ctx, make([]byte, 160)); err != nil {
		t.Fatal(err)
	}
	var entries []recording.Entry
	for i := 0; i < 4; i++ {
		ev, err := s.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Kind {
		case realtime.EventAudio:
			if len(ev.Audio) != 160 || ev.HasTiming {
				t.Fatalf("%+v", ev)
			}
		case realtime.EventCallerTranscriptDelta:
			entries = append(entries, recording.CallerEntry(ev.Text, true, []string{ev.EventID}, ev.HasTiming, ev.StartMS, ev.EndMS))
		case realtime.EventAgentTranscriptDelta:
			p := audio.ItemPlayout{Sent: 20 * time.Millisecond, Confirmed: 20 * time.Millisecond, Estimated: 20 * time.Millisecond}
			e, err := recording.AgentEntry(ev.Text, []string{ev.EventID}, p)
			if err != nil {
				t.Fatal(err)
			}
			entries = append(entries, e)
		case realtime.EventDelegation:
			if err := s.DeclineDelegation(ctx, ev.DelegationID); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected %+v", ev)
		}
	}
	if err := recording.Complete(entries); err != nil {
		t.Fatalf("live_ws two-party transcript incomplete: %v", err)
	}
	if entries[1].Fidelity != recording.FidelityASR || !entries[1].HasProviderTiming {
		t.Fatalf("%+v", entries[1])
	}
	ev, err := s.Next(ctx) // commentary ack is ignored, not a failure
	if err != nil || ev.Kind != realtime.EventIgnored {
		t.Fatalf("%+v %v", ev, err)
	}
	closed, err := s.CloseGracefully(ctx)
	if err != nil || closed.UsageSeconds != 12 {
		t.Fatalf("%+v %v", closed, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.got[0]["type"] != "session.start" {
		t.Fatalf("first client event %v", f.got[0]["type"])
	}
	for _, m := range f.got {
		if m["type"] == "session.commentary.append" && m["delegation_id"] != "del_1" {
			t.Fatalf("%v", m)
		}
		if m["type"] == "response.create" || m["type"] == "session.update" {
			t.Fatalf("no backend/response delegation allowed: %v", m)
		}
	}
	if err := realtime.LiveCapabilities.Check(realtime.Requirements{Format: audio.Format{Encoding: audio.PCMU, SampleRate: 8000}, TwoPartyTranscript: true}); err != nil {
		t.Fatal(err)
	}
}

func TestConnectFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mut := func(change func(s map[string]any)) func(m map[string]any) []byte {
		return func(m map[string]any) []byte {
			s := m["session"].(map[string]any)
			s["id"] = "live_x"
			change(s)
			b, _ := json.Marshal(map[string]any{"type": "session.started", "session": s})
			return b
		}
	}
	cases := map[string]struct {
		started func(m map[string]any) []byte
		want    error
	}{
		"model alias rewritten": {mut(func(s map[string]any) { s["model"] = "other-model" }), realtime.ErrCapability},
		"format changed": {mut(func(s map[string]any) {
			s["audio"] = map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}}
		}), realtime.ErrCapability},
		"responses delegation":  {mut(func(s map[string]any) { s["delegation"] = map[string]any{"type": "responses"} }), realtime.ErrCapability},
		"realtime server reply": {func(map[string]any) []byte { return []byte(`{"type":"session.created","session":{"id":"sess"}}`) }, realtime.ErrProtocol},
		"no session id":         {func(map[string]any) []byte { return []byte(`{"type":"session.started","session":{}}`) }, realtime.ErrProtocol},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Connect(ctx, cfg(startFake(t, &fakeLive{started: tc.started})))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
	t.Run("closed before start", func(t *testing.T) {
		f := &fakeLive{started: func(map[string]any) []byte { return []byte(`{"type":"session.closed","reason":"content"}`) }}
		if _, err := Connect(ctx, cfg(startFake(t, f))); err == nil || !strings.Contains(err.Error(), "before startup") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("error at start", func(t *testing.T) {
		f := &fakeLive{started: func(map[string]any) []byte {
			return []byte(`{"type":"error","event_id":"e","error":{"type":"invalid_request_error","code":"model_not_found","message":"no"}}`)
		}}
		if _, err := Connect(ctx, cfg(startFake(t, f))); err == nil || !strings.Contains(err.Error(), "model_not_found") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("query on endpoint", func(t *testing.T) {
		if _, err := Connect(ctx, cfg("wss://api.example/v1/live/sessions?model=gpt-live-1.1")); err == nil {
			t.Fatal("accepted model query")
		}
	})
	t.Run("realtime emulator is not live", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = c.CloseNow() }()
			_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.created","event_id":"e","session":{"id":"sess_1"}}`))
			_, _, _ = c.Read(r.Context())
		}))
		defer srv.Close()
		if _, err := Connect(ctx, cfg("ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/live/sessions")); !errors.Is(err, realtime.ErrProtocol) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestCloseWithoutClosedIsUnconfirmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_, _, _ = c.Read(r.Context())
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.started","session":{"id":"live_1"}}`))
		_, _, _ = c.Read(r.Context())
		_ = c.Close(websocket.StatusGoingAway, "")
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Connect(ctx, cfg("ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/live/sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseGracefully(ctx); err == nil || !strings.Contains(err.Error(), "usage unconfirmed") {
		t.Fatalf("got %v", err)
	}
}

func TestNotReadyGuards(t *testing.T) {
	var s Session
	ctx := context.Background()
	if err := s.AppendAudio(ctx, make([]byte, 160)); !errors.Is(err, realtime.ErrNotReady) {
		t.Fatal(err)
	}
	if err := s.DeclineDelegation(ctx, "d"); !errors.Is(err, realtime.ErrNotReady) {
		t.Fatal(err)
	}
}
