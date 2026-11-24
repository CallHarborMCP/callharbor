package voicecall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
)

var pcmuLeg = audio.Format{Encoding: audio.PCMU, SampleRate: 8000}

// recorder captures every frame the sink sends to Telnyx.
type recorder struct {
	mu     sync.Mutex
	frames [][]byte
}

func (r *recorder) send(_ context.Context, msg []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, append([]byte(nil), msg...))
	return nil
}

func (r *recorder) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, f := range r.frames {
		var m struct {
			Event string `json:"event"`
		}
		_ = json.Unmarshal(f, &m)
		out = append(out, m.Event)
	}
	return out
}

func newBridge(t *testing.T) (*AudioBridge, *recorder) {
	t.Helper()
	rec := &recorder{}
	vb, err := voicebridge.New(pcmuLeg, pcmuLeg, 5*time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := telnyx.NewBridgeSink(vb, rec.send)
	if err != nil {
		t.Fatal(err)
	}
	ab, err := NewAudioBridge(sink)
	if err != nil {
		t.Fatal(err)
	}
	return ab, rec
}

// TestBargeInSendsTelnyxClear checks that a caller barge-in reaches Telnyx as
// a clear command, so unplayed model audio is dropped on the telephony leg.
func TestBargeInSendsTelnyxClear(t *testing.T) {
	ctx := context.Background()
	ab, rec := newBridge(t)

	ev := realtime.Event{Kind: realtime.EventAudio, ItemID: "item_1", ContentIndex: 0, Audio: make([]byte, 160)}
	for i := range ev.Audio {
		ev.Audio[i] = 0xFF
	}
	ab.EventSink(nil)(ev)
	if err := ab.Play(ctx, ev.Audio); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if err := ab.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	events := rec.events()
	if !containsEvent(events, "clear") {
		t.Fatalf("barge-in did not send Telnyx clear; frames=%v", events)
	}
	if !containsEvent(events, "media") {
		t.Fatalf("agent media was not sent before the clear; frames=%v", events)
	}
}

// TestCallerAudioReachesEngineQueue checks the inbound path: a converted caller
// chunk is delivered to NextCallerAudio with no provider-side loss.
func TestCallerAudioReachesEngineQueue(t *testing.T) {
	ctx := context.Background()
	ab, _ := newBridge(t)
	caller := bytes.Repeat([]byte{0x7F}, 160)

	if err := ab.sink.ToProvider(caller); err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	got, err := ab.NextCallerAudio(ctx)
	if err != nil {
		t.Fatalf("NextCallerAudio: %v", err)
	}
	if !bytes.Equal(got, caller) {
		t.Fatal("caller chunk altered between sink and engine")
	}
}

// TestCallerQueueOverloadIsExplicit checks that a stalled engine gets an
// explicit backpressure error rather than unbounded memory growth.
func TestCallerQueueOverloadIsExplicit(t *testing.T) {
	ab, _ := newBridge(t)
	chunk := bytes.Repeat([]byte{0x7F}, 160)
	var lastErr error
	for i := 0; i < callerQueue+8; i++ {
		lastErr = ab.sink.ToProvider(chunk)
	}
	if !errors.Is(lastErr, telnyx.ErrStreamBackpressure) {
		t.Fatalf("overflow must report backpressure, got %v", lastErr)
	}
}

// TestBridgeCloseEndsCallerLeg checks that Close wakes a pending read with
// ErrBridgeClosed and rejects further caller audio.
func TestBridgeCloseEndsCallerLeg(t *testing.T) {
	ctx := context.Background()
	ab, _ := newBridge(t)
	ab.Close()
	ab.Close()
	if _, err := ab.NextCallerAudio(ctx); !errors.Is(err, ErrBridgeClosed) {
		t.Fatalf("NextCallerAudio after Close: %v", err)
	}
	if err := ab.sink.ToProvider([]byte{1}); !errors.Is(err, ErrBridgeClosed) {
		t.Fatalf("caller audio after Close: %v", err)
	}
}

func containsEvent(events []string, want string) bool {
	for _, e := range events {
		if e == want {
			return true
		}
	}
	return false
}
