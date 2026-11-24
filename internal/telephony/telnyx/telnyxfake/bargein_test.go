package telnyxfake

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/realtime/livews"
	"github.com/CallHarborMCP/callharbor/internal/realtime/realtimews"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
)

type markSignal struct {
	telnyx.MediaSink
	marked chan struct{}
}

func (m markSignal) Mark(name string) error {
	err := m.MediaSink.Mark(name)
	m.marked <- struct{}{}
	return err
}

func TestSignedStreamBargeInClearsThroughLive(t *testing.T) {
	agent, _ := testAudio(pcmuLeg, 4)
	fake := &liveFake{outbound: agent}
	provider := httptest.NewServer(fake.handler(t))
	t.Cleanup(provider.Close)
	endpoint := "ws" + strings.TrimPrefix(provider.URL, "http") + "/v1/live/sessions"
	runBargeIn(t, pcmuLeg, func(ctx context.Context, leg audio.Format) (providerSession, error) {
		return livews.Connect(ctx, livews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-live-1.1", Voice: "marin", Instructions: "Test.", Format: leg})
	})
}

func TestSignedStreamBargeInClearsThroughRealtime(t *testing.T) {
	agent, _ := testAudio(pcmuLeg, 5)
	fake := &realtimeFake{outbound: agent}
	provider := httptest.NewServer(fake.handler(t))
	t.Cleanup(provider.Close)
	endpoint := "ws" + strings.TrimPrefix(provider.URL, "http") + "/v1/realtime"
	runBargeIn(t, pcmuLeg, func(ctx context.Context, leg audio.Format) (providerSession, error) {
		return realtimews.Connect(ctx, realtimews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-realtime-2.1", Voice: "marin", Instructions: "Test.", Format: leg})
	})
}

func TestSignedStreamBargeInSuppressesStreamingLiveAudio(t *testing.T) {
	agent, _ := testAudio(pcmuLeg, 11)
	more, _ := testAudio(pcmuLeg, 12)
	resumed, _ := testAudio(pcmuLeg, 13)
	fake := &liveFake{outbound: agent, followup: more, resumed: resumed}
	provider := httptest.NewServer(fake.handler(t))
	t.Cleanup(provider.Close)
	endpoint := "ws" + strings.TrimPrefix(provider.URL, "http") + "/v1/live/sessions"
	runBargeInStreaming(t, pcmuLeg, true, func(ctx context.Context, leg audio.Format) (providerSession, error) {
		return livews.Connect(ctx, livews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-live-1.1", Voice: "marin", Instructions: "Test.", Format: leg})
	})
}

func TestSignedStreamBargeInSuppressesStreamingRealtimeAudio(t *testing.T) {
	agent, _ := testAudio(pcmuLeg, 7)
	more, _ := testAudio(pcmuLeg, 8)
	fake := &realtimeFake{outbound: agent, followup: more}
	provider := httptest.NewServer(fake.handler(t))
	t.Cleanup(provider.Close)
	endpoint := "ws" + strings.TrimPrefix(provider.URL, "http") + "/v1/realtime"
	runBargeInStreaming(t, pcmuLeg, false, func(ctx context.Context, leg audio.Format) (providerSession, error) {
		return realtimews.Connect(ctx, realtimews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-realtime-2.1", Voice: "marin", Instructions: "Test.", Format: leg})
	})
}

func runBargeInStreaming(t *testing.T, leg audio.Format, resume bool, connect func(ctx context.Context, leg audio.Format) (providerSession, error)) {
	t.Helper()
	priv, pub := newKeys(t)
	now := time.Now()
	v, err := telnyx.NewVerifier(pub, telnyx.DefaultTolerance, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	type cutHandle struct {
		sink   *telnyx.BridgeSink
		bridge *voicebridge.Bridge
		marked chan struct{}
	}
	ready := make(chan cutHandle, 1)
	var me *telnyx.MediaEndpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		me.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	streamURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/media?token=bargein-stream"

	me, err = telnyx.NewMediaEndpoint(v, streamURL, func(start telnyx.Frame, send telnyx.Send) (telnyx.MediaSink, error) {
		b, err := voicebridge.New(start.Format, leg, 5*time.Millisecond, 2*time.Second)
		if err != nil {
			return nil, err
		}
		s, err := telnyx.NewBridgeSink(b, send)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p, err := connect(ctx, leg)
		if err != nil {
			return nil, err
		}
		s.ToProvider = func(in []byte) error { return p.AppendAudio(context.Background(), in) }
		h := cutHandle{sink: s, bridge: b, marked: make(chan struct{}, 4)}
		go func() {
			for {
				ev, err := p.Next(context.Background())
				if err != nil {
					return
				}
				if ev.Kind == realtime.EventCallerTranscriptDelta && resume {
					s.CallerSpoke()
				}
				if ev.Kind == realtime.EventAudio {
					_ = s.AgentAudio(context.Background(), audio.SourceRef{ItemID: ev.ItemID, ContentIndex: ev.ContentIndex}, ev.Audio)
				}
			}
		}()
		ready <- h
		return markSignal{MediaSink: s, marked: h.marked}, nil
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

	start := []byte(`{"event":"start","stream_id":"bargein-stream","sequence_number":"1","start":{"call_control_id":"v3:bargein-stream","client_state":"","media_format":{"encoding":"PCMU","sample_rate":8000,"channels":1}}}`)
	if err := s.Send(ctx, start); err != nil {
		t.Fatal(err)
	}
	_, caller := testAudio(leg, 6)
	if err := s.Send(ctx, mediaFrame(t, "inbound", caller, 320)); err != nil {
		t.Fatal(err)
	}
	var h cutHandle
	select {
	case h = <-ready:
	case <-ctx.Done():
		t.Fatal("stream sink never created")
	}

	recv := func() string {
		b, err := s.Recv(ctx)
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		var m struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m.Event
	}
	if ev := recv(); ev != "media" {
		t.Fatalf("want agent media before cut, got %s", ev)
	}
	if ev := recv(); ev != "mark" {
		t.Fatalf("want mark after agent media, got %s", ev)
	}

	if err := h.sink.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if ev := recv(); ev != "clear" {
		t.Fatalf("want clear after cut, got %s", ev)
	}

	frames := make(chan string, 16)
	go func() {
		defer close(frames)
		for {
			b, err := s.Recv(ctx)
			if err != nil {
				return
			}
			var m struct {
				Event string `json:"event"`
			}
			_ = json.Unmarshal(b, &m)
			frames <- m.Event
		}
	}()

	_, caller2 := testAudio(leg, 9)
	if err := s.Send(ctx, mediaFrame(t, "inbound", caller2, 640)); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-frames:
		if ev == "media" {
			t.Fatal("agent media reached Telnyx after clear")
		}
	case <-time.After(500 * time.Millisecond):
	}

	if !resume {
		return
	}
	_, caller3 := testAudio(leg, 10)
	if err := s.Send(ctx, mediaFrame(t, "inbound", caller3, 960)); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-frames:
		if ev != "media" {
			t.Fatalf("want agent audio to resume after caller transcript, got %s", ev)
		}
	case <-ctx.Done():
		t.Fatal("agent audio never resumed after caller transcript")
	}
}

func runBargeIn(t *testing.T, leg audio.Format, connect func(ctx context.Context, leg audio.Format) (providerSession, error)) {
	t.Helper()
	priv, pub := newKeys(t)
	now := time.Now()
	v, err := telnyx.NewVerifier(pub, telnyx.DefaultTolerance, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	type cutHandle struct {
		sink   *telnyx.BridgeSink
		bridge *voicebridge.Bridge
		marked chan struct{}
	}
	ready := make(chan cutHandle, 1)
	var me *telnyx.MediaEndpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		me.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	streamURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/media?token=bargein"

	me, err = telnyx.NewMediaEndpoint(v, streamURL, func(start telnyx.Frame, send telnyx.Send) (telnyx.MediaSink, error) {
		b, err := voicebridge.New(start.Format, leg, 5*time.Millisecond, 2*time.Second)
		if err != nil {
			return nil, err
		}
		s, err := telnyx.NewBridgeSink(b, send)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p, err := connect(ctx, leg)
		if err != nil {
			return nil, err
		}
		s.ToProvider = func(in []byte) error { return p.AppendAudio(context.Background(), in) }
		h := cutHandle{sink: s, bridge: b, marked: make(chan struct{}, 4)}
		go func() {
			for {
				ev, err := p.Next(context.Background())
				if err != nil {
					return
				}
				if ev.Kind == realtime.EventAudio {
					_ = s.AgentAudio(context.Background(), audio.SourceRef{ItemID: ev.ItemID, ContentIndex: ev.ContentIndex}, ev.Audio)
				}
			}
		}()
		ready <- h
		return markSignal{MediaSink: s, marked: h.marked}, nil
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

	start := []byte(`{"event":"start","stream_id":"bargein-1","sequence_number":"1","start":{"call_control_id":"v3:bargein","client_state":"","media_format":{"encoding":"PCMU","sample_rate":8000,"channels":1}}}`)
	if err := s.Send(ctx, start); err != nil {
		t.Fatal(err)
	}
	_, caller := testAudio(leg, 6)
	if err := s.Send(ctx, mediaFrame(t, "inbound", caller, 320)); err != nil {
		t.Fatal(err)
	}
	var h cutHandle
	select {
	case h = <-ready:
	case <-ctx.Done():
		t.Fatal("stream sink never created")
	}

	recv := func() string {
		b, err := s.Recv(ctx)
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		var m struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m.Event
	}
	if ev := recv(); ev != "media" {
		t.Fatalf("want agent media before cut, got %s", ev)
	}
	if ev := recv(); ev != "mark" {
		t.Fatalf("want mark after agent media, got %s", ev)
	}

	if err := h.sink.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if ev := recv(); ev != "clear" {
		t.Fatalf("want clear after cut, got %s", ev)
	}

	if err := s.Send(ctx, []byte(`{"event":"mark","stream_id":"bargein-1","mark":{"name":"ch-1"}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.marked:
	case <-ctx.Done():
		t.Fatal("pre-cut mark echo never processed")
	}
	items := h.bridge.Items()
	if len(items) != 1 {
		t.Fatalf("want one playout item, got %d", len(items))
	}
	if items[0].Confirmed != 0 || !items[0].Interrupted {
		t.Fatalf("cut item must not count as played: confirmed=%v interrupted=%v", items[0].Confirmed, items[0].Interrupted)
	}
}
