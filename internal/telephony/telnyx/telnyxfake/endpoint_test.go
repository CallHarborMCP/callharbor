package telnyxfake

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
)

var g711Leg = audio.Format{Encoding: audio.PCMU, SampleRate: 8000}

type providerSession interface {
	AppendAudio(ctx context.Context, b []byte) error
	Next(ctx context.Context) (realtime.Event, error)
}

type liveFake struct {
	mu       sync.Mutex
	inbound  [][]byte
	outbound []byte
}

func (f *liveFake) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		ctx := r.Context()
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
			switch m["type"] {
			case "session.start":
				s, _ := m["session"].(map[string]any)
				s["id"], s["status"] = "live_e2e", "active"
				sess, _ := json.Marshal(s)
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.started","event_id":"evt_started","client_event_id":"ch_session_start","session":`+string(sess)+`}`))
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.output_audio.delta","delta":"`+base64.StdEncoding.EncodeToString(f.outbound)+`"}`))
			case "session.input_audio.append":
				raw, err := base64.StdEncoding.DecodeString(m["audio"].(string))
				if err != nil {
					t.Errorf("provider got bad base64: %v", err)
					return
				}
				f.mu.Lock()
				f.inbound = append(f.inbound, raw)
				f.mu.Unlock()
			case "session.close":
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.closed","event_id":"evt_closed","reason":"close_requested","usage":{"seconds":1}}`))
			}
		}
	}
}

func (f *liveFake) received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.inbound...)
}

type realtimeFake struct {
	mu       sync.Mutex
	inbound  [][]byte
	outbound []byte
}

func (f *realtimeFake) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("model") == "" {
			http.Error(w, "missing model", http.StatusBadRequest)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		ctx := r.Context()
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created","event_id":"evt_created","session":{"id":"sess_e2e","type":"realtime"}}`))
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
			switch m["type"] {
			case "session.update":
				s, _ := m["session"].(map[string]any)
				upd, _ := json.Marshal(map[string]any{"type": "session.updated", "event_id": "evt_updated", "session": s})
				_ = c.Write(ctx, websocket.MessageText, upd)
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.output_audio.delta","event_id":"evt_delta","response_id":"resp_e2e","item_id":"item_e2e","output_index":0,"content_index":0,"delta":"`+base64.StdEncoding.EncodeToString(f.outbound)+`"}`))
			case "input_audio_buffer.append":
				raw, err := base64.StdEncoding.DecodeString(m["audio"].(string))
				if err != nil {
					t.Errorf("provider got bad base64: %v", err)
					return
				}
				f.mu.Lock()
				f.inbound = append(f.inbound, raw)
				f.mu.Unlock()
			}
		}
	}
}

func (f *realtimeFake) received() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.inbound...)
}

func mediaFrame(t *testing.T, track string, payload []byte, ts int) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"event": "media", "stream_id": "e2e-1", "sequence_number": fmt.Sprint(ts),
		"media": map[string]any{"track": track, "chunk": "1", "timestamp": fmt.Sprint(ts), "payload": base64.StdEncoding.EncodeToString(payload)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func runSignedCall(t *testing.T, connect func(ctx context.Context) (providerSession, error), agent, caller []byte, received func() [][]byte) {
	t.Helper()
	priv, pub := newKeys(t)
	now := time.Now()
	v, err := telnyx.NewVerifier(pub, telnyx.DefaultTolerance, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	dtmf := make(chan string, 1)
	var me *telnyx.MediaEndpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		me.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	streamURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/media?token=e2e"

	me, err = telnyx.NewMediaEndpoint(v, streamURL, func(start telnyx.Frame, send telnyx.Send) (telnyx.MediaSink, error) {
		b, err := voicebridge.New(start.Format, g711Leg, 5*time.Millisecond, 2*time.Second)
		if err != nil {
			return nil, err
		}
		sink, err := telnyx.NewBridgeSink(b, send)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s, err := connect(ctx)
		if err != nil {
			return nil, err
		}
		sink.ToProvider = func(p []byte) error { return s.AppendAudio(context.Background(), p) }
		sink.OnDTMF = func(d string) {
			select {
			case dtmf <- d:
			default:
			}
		}
		go func() {
			for {
				ev, err := s.Next(context.Background())
				if err != nil {
					return
				}
				if ev.Kind == realtime.EventAudio {
					_ = sink.AgentAudio(context.Background(), audio.SourceRef{ItemID: ev.ItemID, ContentIndex: ev.ContentIndex}, ev.Audio)
				}
			}
		}()
		return sink, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := DialMedia(ctx, streamURL, priv, now)
	if err != nil {
		t.Fatalf("signed dial rejected: %v", err)
	}
	defer func() { _ = s.Close() }()

	start := []byte(`{"event":"start","stream_id":"e2e-1","sequence_number":"1","start":{"call_control_id":"v3:e2e","client_state":"","media_format":{"encoding":"PCMU","sample_rate":8000,"channels":1}}}`)
	if err := s.Send(ctx, start); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(ctx, mediaFrame(t, "inbound", caller, 320)); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(ctx, []byte(`{"event":"dtmf","stream_id":"e2e-1","dtmf":{"digit":"7"}}`)); err != nil {
		t.Fatal(err)
	}

	got, err := s.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Event string `json:"event"`
		Media struct {
			Payload string `json:"payload"`
		} `json:"media"`
	}
	if err := json.Unmarshal(got, &out); err != nil || out.Event != "media" {
		t.Fatalf("want agent media frame, got %s (%v)", got, err)
	}
	gotAgent, err := base64.StdEncoding.DecodeString(out.Media.Payload)
	if err != nil || !bytes.Equal(gotAgent, agent) {
		t.Fatalf("agent audio not byte-exact through Telnyx: %v", err)
	}

	select {
	case d := <-dtmf:
		if d != "7" {
			t.Fatalf("dtmf %q, want 7", d)
		}
	case <-ctx.Done():
		t.Fatal("dtmf never reached the call layer")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		in := received()
		if len(in) >= 1 {
			if !bytes.Equal(in[0], caller) {
				t.Fatal("caller audio not byte-exact to provider")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("provider received no inbound audio")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testAudio(seed int) (agent, caller []byte) {
	agent = make([]byte, 160)
	caller = make([]byte, 160)
	for i := range agent {
		agent[i] = audio.LinearToUlaw(int16(i * (96 + seed)))
		caller[i] = audio.LinearToUlaw(int16(i * (64 + seed)))
	}
	return agent, caller
}

func TestSignedStreamBridgesAudioBothWaysThroughLive(t *testing.T) {
	agent, caller := testAudio(0)
	fake := &liveFake{outbound: agent}
	provider := httptest.NewServer(fake.handler(t))
	t.Cleanup(provider.Close)
	endpoint := "ws" + strings.TrimPrefix(provider.URL, "http") + "/v1/live/sessions"

	runSignedCall(t, func(ctx context.Context) (providerSession, error) {
		s, err := livews.Connect(ctx, livews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-live-1.1", Voice: "marin", Instructions: "Test.", Format: g711Leg})
		if err != nil {
			return nil, err
		}
		return s, nil
	}, agent, caller, fake.received)
}

func TestSignedStreamBridgesAudioBothWaysThroughRealtime(t *testing.T) {
	agent, caller := testAudio(1)
	fake := &realtimeFake{outbound: agent}
	provider := httptest.NewServer(fake.handler(t))
	t.Cleanup(provider.Close)
	endpoint := "ws" + strings.TrimPrefix(provider.URL, "http") + "/v1/realtime"

	runSignedCall(t, func(ctx context.Context) (providerSession, error) {
		s, err := realtimews.Connect(ctx, realtimews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-realtime-2.1", Voice: "marin", Instructions: "Test.", Format: g711Leg})
		if err != nil {
			return nil, err
		}
		return s, nil
	}, agent, caller, fake.received)
}
