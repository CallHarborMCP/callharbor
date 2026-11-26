package telnyxfake

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/realtime/livews"
	"github.com/CallHarborMCP/callharbor/internal/realtime/realtimews"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
)

// liveHangoverFrames is the number of silent 20 ms frames the detector needs
// before it reports the end of caller speech.
const liveHangoverFrames = int(audio.VADHangover / audio.VADFrame)

// liveTurnResumeAppend is the 1-based provider append that completes the
// caller's turn in runBargeInStreaming. Appends are: 1 tone (before the cut),
// 2 tone (after the cut, the Live fake's cut-turn reply arrives here),
// 3 tone (speech onset completes at frame 3, since VADOnset is 3 frames), then
// liveHangoverFrames silent frames. The end edge is the last of them, and the
// Live fake replies on that append, so the reply is generated only after the
// caller has stopped talking.
const liveTurnResumeAppend = 3 + liveHangoverFrames

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

// The Live path reopens the anonymous turn only after the caller's speech ends,
// as the engine does on the detector's end edge. The provider's transcript is
// never a trigger, so the reply the fake makes during the cut turn stays gated.
func TestSignedStreamBargeInSuppressesStreamingLiveAudio(t *testing.T) {
	agent, _ := testAudio(pcmuLeg, 11)
	more, _ := testAudio(pcmuLeg, 12)
	resumed, _ := testAudio(pcmuLeg, 13)
	fake := &liveFake{outbound: agent, followup: more, resumed: resumed, resumeAt: liveTurnResumeAppend}
	provider := httptest.NewServer(fake.handler(t))
	t.Cleanup(provider.Close)
	endpoint := "ws" + strings.TrimPrefix(provider.URL, "http") + "/v1/live/sessions"
	runBargeInStreaming(t, pcmuLeg, true, resumed, func(ctx context.Context, leg audio.Format) (providerSession, error) {
		return livews.Connect(ctx, livews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-live-1.1", Voice: "marin", Instructions: "Test.", Format: leg})
	})
}

// realtime_ws has a native speech-start event and no local turn gate. Its
// streamed output is cut per item, so nothing after the cut may reach Telnyx.
func TestSignedStreamBargeInSuppressesStreamingRealtimeAudio(t *testing.T) {
	agent, _ := testAudio(pcmuLeg, 7)
	more, _ := testAudio(pcmuLeg, 8)
	fake := &realtimeFake{outbound: agent, followup: more}
	provider := httptest.NewServer(fake.handler(t))
	t.Cleanup(provider.Close)
	endpoint := "ws" + strings.TrimPrefix(provider.URL, "http") + "/v1/realtime"
	runBargeInStreaming(t, pcmuLeg, false, nil, func(ctx context.Context, leg audio.Format) (providerSession, error) {
		return realtimews.Connect(ctx, realtimews.Config{Endpoint: endpoint, APIKey: "sk-test", Model: "gpt-realtime-2.1", Voice: "marin", Instructions: "Test.", Format: leg})
	})
}

// toneFrame is one 20 ms G.711 frame of a 440 Hz tone well above the detector
// threshold. silenceFrame is one frame of digital silence.
func toneFrame(leg audio.Format) []byte {
	out := make([]byte, 160)
	for i := range out {
		out[i] = encodeG711(leg, int16(8000*math.Sin(2*math.Pi*440*float64(i)/8000)))
	}
	return out
}

func silenceFrame(leg audio.Format) []byte {
	out := make([]byte, 160)
	for i := range out {
		out[i] = encodeG711(leg, 0)
	}
	return out
}

// decodeOut returns the Telnyx event name and, for media frames, the payload.
func decodeOut(t *testing.T, raw []byte) (string, []byte) {
	t.Helper()
	var m struct {
		Event string `json:"event"`
		Media struct {
			Payload string `json:"payload"`
		} `json:"media"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Event != "media" {
		return m.Event, nil
	}
	b, err := base64.StdEncoding.DecodeString(m.Media.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return m.Event, b
}

// runBargeInStreaming drives a signed stream through a real BridgeSink while
// the provider fake keeps streaming agent audio during and after the cut.
//
// When localTurns is true (live_ws, no native speech-start event), the sink's
// caller speech edges drive ReopenTurn exactly as the engine's speech pump does:
// on the end edge only, never on a provider transcript. expectResume is the
// payload the provider sends once the caller has finished; it must be the first
// agent audio to reach Telnyx after clear. When localTurns is false, nothing
// reopens the turn and expectResume is nil.
func runBargeInStreaming(t *testing.T, leg audio.Format, localTurns bool, expectResume []byte, connect func(ctx context.Context, leg audio.Format) (providerSession, error)) {
	t.Helper()
	priv, pub := newKeys(t)
	now := time.Now()
	v, err := telnyx.NewVerifier(pub, telnyx.DefaultTolerance, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	type cutHandle struct {
		sink       *telnyx.BridgeSink
		bridge     *voicebridge.Bridge
		marked     chan struct{}
		processed  chan struct{}
		speechEnds *atomic.Int64
	}
	ready := make(chan cutHandle, 1)
	var me *telnyx.MediaEndpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		me.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	streamURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/media?token=bargein-stream"

	me, err = telnyx.NewMediaEndpoint(v, streamURL, func(_ context.Context, start telnyx.Frame, send telnyx.Send) (telnyx.MediaSink, error) {
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
		h := cutHandle{sink: s, bridge: b, marked: make(chan struct{}, 4), processed: make(chan struct{}, 32), speechEnds: &atomic.Int64{}}
		if localTurns {
			// Mirrors voicecall.AudioBridge → voice.pumpSpeech: only an end edge reopens.
			s.OnCallerSpeech = func(ev telnyx.SpeechEvent) {
				if ev.Started {
					return
				}
				h.speechEnds.Add(1)
				s.ReopenTurn()
			}
		}
		go func() {
			for {
				ev, err := p.Next(context.Background())
				if err != nil {
					return
				}
				if ev.Kind == realtime.EventAudio {
					_ = s.AgentAudio(context.Background(), audio.SourceRef{ItemID: ev.ItemID, ContentIndex: ev.ContentIndex}, ev.Audio)
					h.processed <- struct{}{}
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
	// Provider append 1: caller speech before the cut.
	if err := s.Send(ctx, mediaFrame(t, "inbound", toneFrame(leg), 320)); err != nil {
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
		ev, _ := decodeOut(t, b)
		return ev
	}
	if ev := recv(); ev != "media" {
		t.Fatalf("want agent media before cut, got %s", ev)
	}
	if ev := recv(); ev != "mark" {
		t.Fatalf("want mark after agent media, got %s", ev)
	}

	if _, err := h.sink.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if ev := recv(); ev != "clear" {
		t.Fatalf("want clear after cut, got %s", ev)
	}

	frames := make(chan []byte, 16)
	go func() {
		defer close(frames)
		for {
			b, err := s.Recv(ctx)
			if err != nil {
				return
			}
			select {
			case frames <- b:
			case <-ctx.Done():
				return
			}
		}
	}()

	send := func(payload []byte, ts int) {
		t.Helper()
		if err := s.Send(ctx, mediaFrame(t, "inbound", payload, ts)); err != nil {
			t.Fatal(err)
		}
	}
	// awaitProcessed blocks until n provider audio events have been fully handled
	// by the sink. Pre-cut output counts as one, so after the first post-cut frame
	// the fake's streamed reply is known to have been gated or sent before the
	// test sends anything else. This is a happens-before barrier, not a sleep.
	awaitProcessed := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			select {
			case <-h.processed:
			case <-ctx.Done():
				t.Fatal("provider audio event never processed")
			}
		}
	}

	// Provider append 2: the Live and Realtime fakes stream a reply during the
	// cut turn. Both must be suppressed.
	send(toneFrame(leg), 640)
	awaitProcessed(2)

	if expectResume == nil {
		// No local turn reopening: nothing may reach Telnyx while the cut
		// item keeps streaming. The bounded window confirms an absence.
		select {
		case raw := <-frames:
			if ev, _ := decodeOut(t, raw); ev == "media" {
				t.Fatal("agent media reached Telnyx after clear")
			}
		case <-time.After(500 * time.Millisecond):
		}
		return
	}

	// Live: finish the caller's turn. Onset completes on this loud frame, then a
	// full hangover of silence ends the speech on the detector's end edge, which
	// reopens the turn before the provider append that triggers the reply.
	send(toneFrame(leg), 960)
	for i := 0; i < int(audio.VADHangover/audio.VADFrame); i++ {
		send(silenceFrame(leg), 1280+320*i)
	}

	var raw []byte
	select {
	case raw = <-frames:
	case <-ctx.Done():
		t.Fatal("agent audio never resumed after caller speech ended")
	}
	ev, payload := decodeOut(t, raw)
	// The first agent audio after clear must be the post-turn reply. If the cut
	// turn's streamed reply had reached Telnyx, its payload would come first.
	if ev != "media" || !bytes.Equal(payload, expectResume) {
		t.Fatalf("first agent audio after clear is %s (%d bytes), want the post-turn reply only", ev, len(payload))
	}
	if got := h.speechEnds.Load(); got != 1 {
		t.Fatalf("caller speech end edges = %d, want exactly 1", got)
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

	me, err = telnyx.NewMediaEndpoint(v, streamURL, func(_ context.Context, start telnyx.Frame, send telnyx.Send) (telnyx.MediaSink, error) {
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

	if _, err := h.sink.Interrupt(ctx); err != nil {
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
