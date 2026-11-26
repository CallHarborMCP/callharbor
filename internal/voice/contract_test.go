package voice_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	"github.com/CallHarborMCP/callharbor/internal/voice"
)

var pcmu = audio.Format{Encoding: audio.PCMU, SampleRate: 8000}

type recordingBridge struct {
	mu         sync.Mutex
	played     int
	interrupts int
}

func (b *recordingBridge) NextCallerAudio(ctx context.Context) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *recordingBridge) Play(_ context.Context, pcm []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.played += len(pcm)
	return nil
}

func (b *recordingBridge) Interrupt(context.Context) (voice.Cut, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.interrupts++
	return voice.Cut{}, nil
}

func (b *recordingBridge) counts() (played, interrupts int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.played, b.interrupts
}

// fakeRealtimeProvider speaks the documented Realtime GA handshake, emits the
// given events, then drops the socket without a close event.
func fakeRealtimeProvider(t *testing.T, events ...string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created","session":{"id":"s"}}`))
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.updated","session":{"audio":{"input":{"format":{"type":"audio/pcmu"}},"output":{"format":{"type":"audio/pcmu"}}}}}`))
		for _, ev := range events {
			_ = c.Write(ctx, websocket.MessageText, []byte(ev))
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"
}

// fakeLiveProvider speaks the documented GPT-Live startup: client-first
// session.start, answered with session.started, then the given events.
func fakeLiveProvider(t *testing.T, events ...string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		ctx := r.Context()
		_, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		if !strings.Contains(string(b), `"type":"session.start"`) {
			return
		}
		var start struct {
			Session struct {
				Model string `json:"model"`
			} `json:"session"`
		}
		if err := json.Unmarshal(b, &start); err != nil {
			return
		}
		started, _ := json.Marshal(map[string]any{
			"type":     "session.started",
			"event_id": "evt_started",
			"session":  map[string]any{"id": "live_1", "model": start.Session.Model, "status": "active"},
		})
		_ = c.Write(ctx, websocket.MessageText, started)
		for _, ev := range events {
			_ = c.Write(ctx, websocket.MessageText, []byte(ev))
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/live/sessions"
}

// fakeLiveHoldProvider keeps the Live session open after startup and answers
// session.close with session.closed carrying confirmed usage. It records
// whether the engine asked for a graceful close.
func fakeLiveHoldProvider(t *testing.T, closeSeen chan<- struct{}) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		ctx := r.Context()
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.started","event_id":"evt_started","session":{"id":"live_1","model":"model-1","status":"active"}}`))
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			if strings.Contains(string(b), `"type":"session.close"`) {
				closeSeen <- struct{}{}
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.closed","event_id":"evt_closed","reason":"close_requested","usage":{"seconds":7}}`))
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/live/sessions"
}

func liveAudio() string {
	return `{"type":"session.output_audio.delta","delta":"` + base64.StdEncoding.EncodeToString(make([]byte, 160)) + `"}`
}

func profileFor(proto realtime.Protocol, endpoint string) realtime.Profile {
	return realtime.Profile{
		Protocol: proto,
		Endpoint: endpoint,
		APIKey:   "sk-test",
		Model:    "model-1",
		Voice:    "marin",
		Format:   pcmu,
	}
}

func dialers() realtime.Dialers {
	return realtime.Dialers{
		realtime.ProtocolRealtime: realtimews.Dialer,
		realtime.ProtocolLive:     livews.Dialer,
	}
}

func runCall(t *testing.T, proto realtime.Protocol, endpoint string) (voice.Result, []realtime.EventKind, *recordingBridge, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := dialers().Dial(ctx, profileFor(proto, endpoint), realtime.Requirements{Format: pcmu})
	if err != nil {
		t.Fatalf("dial %s: %v", proto, err)
	}
	br := &recordingBridge{}
	var kinds []realtime.EventKind
	res, runErr := voice.Run(ctx, sess, br, func(ev realtime.Event) { kinds = append(kinds, ev.Kind) })
	return res, kinds, br, runErr
}

func TestRealtimeCallContract(t *testing.T) {
	t.Run("speech start interrupts and audio plays", func(t *testing.T) {
		ep := fakeRealtimeProvider(t,
			`{"type":"response.output_audio.delta","event_id":"e1","response_id":"r1","item_id":"i1","output_index":0,"content_index":0,"delta":"`+base64.StdEncoding.EncodeToString(make([]byte, 160))+`"}`,
			`{"type":"input_audio_buffer.speech_started","event_id":"e2","audio_start_ms":10,"item_id":"i2"}`,
		)
		res, kinds, br, err := runCall(t, realtime.ProtocolRealtime, ep)
		if !errors.Is(err, realtime.ErrDisconnected) {
			t.Fatalf("drop must surface as ErrDisconnected, got %v", err)
		}
		if res.Closed {
			t.Fatal("realtime has no close event; a drop must never be a clean close")
		}
		played, interrupts := br.counts()
		if played != 160 || interrupts != 1 {
			t.Fatalf("played=%d interrupts=%d kinds=%v", played, interrupts, kinds)
		}
	})

	t.Run("provider error ends the call", func(t *testing.T) {
		ep := fakeRealtimeProvider(t, `{"type":"error","event_id":"e","error":{"type":"invalid_request_error","code":"rate_limit","message":"slow"}}`)
		res, _, _, err := runCall(t, realtime.ProtocolRealtime, ep)
		if !errors.Is(err, voice.ErrProviderError) || res.Closed {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
}

func TestLiveCallContract(t *testing.T) {
	t.Run("documented close is the only clean finish", func(t *testing.T) {
		ep := fakeLiveProvider(t,
			liveAudio(),
			`{"type":"session.input_transcript.delta","event_id":"i1","delta":"Hi, I need a table.","start_ms":1,"end_ms":2}`,
			`{"type":"session.closed","event_id":"evt_closed","reason":"done","usage":{"seconds":3}}`,
		)
		res, kinds, br, err := runCall(t, realtime.ProtocolLive, ep)
		if err != nil || !res.Closed {
			t.Fatalf("documented close must be clean: res=%+v err=%v", res, err)
		}
		played, _ := br.counts()
		if played != 160 {
			t.Fatalf("played=%d kinds=%v", played, kinds)
		}
		if !containsKind(kinds, realtime.EventCallerTranscriptDelta) || !containsKind(kinds, realtime.EventClosed) {
			t.Fatalf("kinds=%v", kinds)
		}
	})

	t.Run("drop without close is not a clean finish", func(t *testing.T) {
		ep := fakeLiveProvider(t, liveAudio())
		res, _, _, err := runCall(t, realtime.ProtocolLive, ep)
		if !errors.Is(err, realtime.ErrDisconnected) || res.Closed {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})

	t.Run("provider error ends the call", func(t *testing.T) {
		ep := fakeLiveProvider(t, `{"type":"error","event_id":"e","error":{"message":"boom"}}`)
		res, _, _, err := runCall(t, realtime.ProtocolLive, ep)
		if !errors.Is(err, voice.ErrProviderError) || res.Closed {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
}

func TestLocalHangupFinalizesLiveWithConfirmedUsage(t *testing.T) {
	closeSeen := make(chan struct{}, 1)
	ep := fakeLiveHoldProvider(t, closeSeen)
	ctx, cancel := context.WithCancel(context.Background())
	sess, err := dialers().Dial(context.Background(), profileFor(realtime.ProtocolLive, ep), realtime.Requirements{Format: pcmu})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	time.AfterFunc(50*time.Millisecond, cancel)
	res, err := voice.Run(ctx, sess, &recordingBridge{}, nil)
	if err != nil {
		t.Fatalf("err = %v, want nil after confirmed graceful close", err)
	}
	select {
	case <-closeSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never received session.close on local hangup")
	}
	if !res.Closed || !res.UsageConfirmed || res.UsageSeconds != 7 {
		t.Fatalf("res=%+v, want clean close with confirmed usage 7s", res)
	}
}

func TestLocalHangupRealtimeHasNoGracefulClose(t *testing.T) {
	ep := fakeRealtimeProvider(t)
	ctx, cancel := context.WithCancel(context.Background())
	sess, err := dialers().Dial(context.Background(), profileFor(realtime.ProtocolRealtime, ep), realtime.Requirements{Format: pcmu})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	time.AfterFunc(50*time.Millisecond, cancel)
	res, _ := voice.Run(ctx, sess, &recordingBridge{}, nil)
	if res.Closed || res.UsageConfirmed {
		t.Fatalf("realtime local hangup reported closed/confirmed usage: %+v", res)
	}
}

func TestAlternateAdapterBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("live profile never reaches a realtime endpoint", func(t *testing.T) {
		var mu sync.Mutex
		var sawUpdate bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = c.CloseNow() }()
			_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.created","session":{"id":"s"}}`))
			if _, b, err := c.Read(r.Context()); err == nil && strings.Contains(string(b), `"session.update"`) {
				mu.Lock()
				sawUpdate = true
				mu.Unlock()
			}
		}))
		t.Cleanup(srv.Close)
		ep := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime"
		if _, err := dialers().Dial(ctx, profileFor(realtime.ProtocolLive, ep), realtime.Requirements{Format: pcmu}); err == nil {
			t.Fatal("live profile must fail closed against a non-live endpoint")
		}
		mu.Lock()
		defer mu.Unlock()
		if sawUpdate {
			t.Fatal("live profile fell back to the realtime handshake")
		}
	})

	t.Run("unregistered protocol fails closed", func(t *testing.T) {
		d := realtime.Dialers{realtime.ProtocolRealtime: realtimews.Dialer}
		if _, err := d.Dial(ctx, profileFor(realtime.ProtocolLive, "wss://example.invalid/v1/live/sessions"), realtime.Requirements{Format: pcmu}); !errors.Is(err, realtime.ErrNoAdapter) {
			t.Fatalf("got %v", err)
		}
	})
}

func containsKind(kinds []realtime.EventKind, want realtime.EventKind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}
