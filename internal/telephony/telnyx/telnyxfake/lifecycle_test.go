package telnyxfake

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func liveLifecycleFake(t *testing.T, agent []byte) http.HandlerFunc {
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
				s["id"], s["status"] = "live_life", "active"
				sess, _ := json.Marshal(s)
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.started","event_id":"evt_started","client_event_id":"ch_session_start","session":`+string(sess)+`}`))
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.output_audio.delta","delta":"`+base64.StdEncoding.EncodeToString(agent)+`"}`))
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.output_transcript.delta","event_id":"o1","delta":"Hello.","start_ms":0,"end_ms":900}`))
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.input_transcript.delta","event_id":"i1","delta":"Hi.","start_ms":100,"end_ms":400}`))
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"error","event_id":"e1","error":{"code":"unknown_parameter","message":"bad field"}}`))
			case "session.close":
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.closed","event_id":"evt_closed","reason":"close_requested","usage":{"seconds":12}}`))
			}
		}
	}
}

func TestSignedStreamLifecycleSurfacesProviderEventsThroughLive(t *testing.T) {
	agent, caller := testAudio(pcmuLeg, 7)
	provider := httptest.NewServer(liveLifecycleFake(t, agent))
	t.Cleanup(provider.Close)
	endpoint := "ws" + strings.TrimPrefix(provider.URL, "http") + "/v1/live/sessions"

	priv, pub := newKeys(t)
	now := time.Now()
	v, err := telnyx.NewVerifier(pub, telnyx.DefaultTolerance, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	var me *telnyx.MediaEndpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { me.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	streamURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/media?token=life"

	events := make(chan realtime.Event, 16)
	closedCh := make(chan realtime.Event, 1)
	me, err = telnyx.NewMediaEndpoint(v, streamURL, func(start telnyx.Frame, send telnyx.Send) (telnyx.MediaSink, error) {
		b, err := voicebridge.New(start.Format, pcmuLeg, 5*time.Millisecond, 2*time.Second)
		if err != nil {
			return nil, err
		}
		s, err := telnyx.NewBridgeSink(b, send)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p, err := livews.Connect(ctx, livews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-live-1.1", Voice: "marin", Instructions: "Test.", Format: pcmuLeg})
		if err != nil {
			return nil, err
		}
		s.ToProvider = func(in []byte) error { return p.AppendAudio(context.Background(), in) }
		go func() {
			for {
				ev, err := p.Next(context.Background())
				if err != nil {
					return
				}
				if ev.Kind == realtime.EventAudio {
					_ = s.AgentAudio(context.Background(), audio.SourceRef{ItemID: ev.ItemID, ContentIndex: ev.ContentIndex}, ev.Audio)
				}
				if ev.Kind != realtime.EventAudio {
					events <- ev
				}
				if ev.Kind == realtime.EventError {
					cl, err := p.CloseGracefully(context.Background())
					if err == nil {
						closedCh <- cl
					}
					return
				}
			}
		}()
		return s, nil
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
	start := []byte(`{"event":"start","stream_id":"life-1","sequence_number":"1","start":{"call_control_id":"v3:life","client_state":"","media_format":{"encoding":"PCMU","sample_rate":8000,"channels":1}}}`)
	if err := s.Send(ctx, start); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(ctx, mediaFrame(t, "inbound", caller, 320)); err != nil {
		t.Fatal(err)
	}

	want := map[realtime.EventKind]bool{
		realtime.EventAgentTranscriptDelta:  false,
		realtime.EventCallerTranscriptDelta: false,
		realtime.EventError:                 false,
	}
	deadline := time.After(5 * time.Second)
	for {
		done := true
		for _, ok := range want {
			if !ok {
				done = false
			}
		}
		if done {
			break
		}
		select {
		case ev := <-events:
			if ev.Kind == realtime.EventAgentTranscriptDelta && ev.Text == "Hello." && ev.EndMS == 900 {
				want[ev.Kind] = true
			}
			if ev.Kind == realtime.EventCallerTranscriptDelta && ev.Text == "Hi." {
				want[ev.Kind] = true
			}
			if ev.Kind == realtime.EventError && ev.ErrCode == "unknown_parameter" {
				want[ev.Kind] = true
			}
		case <-deadline:
			t.Fatalf("provider events not surfaced to call layer: %+v", want)
		}
	}

	select {
	case cl := <-closedCh:
		if cl.Kind != realtime.EventClosed || cl.UsageSeconds != 12 || cl.CloseReason != "close_requested" {
			t.Fatalf("close not confirmed with usage: %+v", cl)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session.close never confirmed")
	}
}

func realtimeLifecycleFake(t *testing.T, agent []byte, closed chan<- struct{}) http.HandlerFunc {
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
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created","event_id":"evt_created","session":{"id":"sess_life","type":"realtime"}}`))
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				select {
				case closed <- struct{}{}:
				default:
				}
				return
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Errorf("provider got invalid JSON: %v", err)
				return
			}
			if m["type"] != "session.update" {
				continue
			}
			s, _ := m["session"].(map[string]any)
			upd, _ := json.Marshal(map[string]any{"type": "session.updated", "event_id": "evt_updated", "session": s})
			_ = c.Write(ctx, websocket.MessageText, upd)
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.output_audio.delta","event_id":"evt_delta","response_id":"resp_life","item_id":"item_life","output_index":0,"content_index":0,"delta":"`+base64.StdEncoding.EncodeToString(agent)+`"}`))
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.output_audio_transcript.delta","event_id":"t1","response_id":"resp_life","item_id":"item_life","output_index":0,"content_index":0,"delta":"Hello."}`))
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.done","event_id":"evt_done","response":{"id":"resp_life","status":"completed","usage":{"total_tokens":42}}}`))
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"error","event_id":"e1","error":{"type":"invalid_request_error","code":"unknown_parameter","message":"bad field"}}`))
		}
	}
}

func TestSignedStreamLifecycleSurfacesProviderEventsThroughRealtime(t *testing.T) {
	agent, caller := testAudio(pcmuLeg, 8)
	closed := make(chan struct{}, 1)
	provider := httptest.NewServer(realtimeLifecycleFake(t, agent, closed))
	t.Cleanup(provider.Close)
	endpoint := "ws" + strings.TrimPrefix(provider.URL, "http") + "/v1/realtime"

	priv, pub := newKeys(t)
	now := time.Now()
	v, err := telnyx.NewVerifier(pub, telnyx.DefaultTolerance, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	var me *telnyx.MediaEndpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { me.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	streamURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/media?token=rtlife"

	events := make(chan realtime.Event, 16)
	closedCh := make(chan struct{}, 1)
	me, err = telnyx.NewMediaEndpoint(v, streamURL, func(start telnyx.Frame, send telnyx.Send) (telnyx.MediaSink, error) {
		b, err := voicebridge.New(start.Format, pcmuLeg, 5*time.Millisecond, 2*time.Second)
		if err != nil {
			return nil, err
		}
		s, err := telnyx.NewBridgeSink(b, send)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p, err := realtimews.Connect(ctx, realtimews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-realtime-2.1", Voice: "marin", Instructions: "Test.", Format: pcmuLeg})
		if err != nil {
			return nil, err
		}
		s.ToProvider = func(in []byte) error { return p.AppendAudio(context.Background(), in) }
		go func() {
			for {
				ev, err := p.Next(context.Background())
				if err != nil {
					return
				}
				if ev.Kind == realtime.EventAudio {
					_ = s.AgentAudio(context.Background(), audio.SourceRef{ItemID: ev.ItemID, ContentIndex: ev.ContentIndex}, ev.Audio)
				}
				if ev.Kind != realtime.EventAudio {
					events <- ev
				}
				if ev.Kind == realtime.EventError {
					if err := p.Close(); err == nil {
						closedCh <- struct{}{}
					}
					return
				}
			}
		}()
		return s, nil
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
	start := []byte(`{"event":"start","stream_id":"rtlife-1","sequence_number":"1","start":{"call_control_id":"v3:rtlife","client_state":"","media_format":{"encoding":"PCMU","sample_rate":8000,"channels":1}}}`)
	if err := s.Send(ctx, start); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(ctx, mediaFrame(t, "inbound", caller, 320)); err != nil {
		t.Fatal(err)
	}

	gotTranscript, gotUsage, gotError := false, false, false
	deadline := time.After(5 * time.Second)
	for !gotTranscript || !gotUsage || !gotError {
		select {
		case ev := <-events:
			switch {
			case ev.Kind == realtime.EventAgentTranscriptDelta && ev.Text == "Hello." && ev.ItemID == "item_life":
				gotTranscript = true
			case ev.Kind == realtime.EventResponseDone && ev.ResponseStatus == "completed" && ev.UsageTokens == 42:
				gotUsage = true
			case ev.Kind == realtime.EventError && ev.ErrCode == "unknown_parameter":
				gotError = true
			}
		case <-deadline:
			t.Fatalf("provider events not surfaced to call layer: transcript=%v usage=%v error=%v", gotTranscript, gotUsage, gotError)
		}
	}

	select {
	case <-closedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("realtime socket never closed by the bridge")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never observed the realtime socket close")
	}
}
