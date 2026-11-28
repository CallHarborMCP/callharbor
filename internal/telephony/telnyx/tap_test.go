package telnyx_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
)

// recTap records every tap call so a test can assert placement and order.
type recTap struct {
	mu     sync.Mutex
	agent  []tapAgent
	cuts   []time.Time
	caller []uint32
}

type tapAgent struct {
	playStart time.Time
	at        time.Time
	n         int
}

func (r *recTap) CallerPayload(ts uint32, _ []byte, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.caller = append(r.caller, ts)
}

func (r *recTap) AgentPayload(playStart time.Time, payload []byte, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agent = append(r.agent, tapAgent{playStart: playStart, at: at, n: len(payload)})
}

func (r *recTap) Cut(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cuts = append(r.cuts, at)
}

// newTapSink builds a BridgeSink over a G.711 bridge with a fixed clock.
func newTapSink(t *testing.T, tap telnyx.AudioTap, now *time.Time) *telnyx.BridgeSink {
	t.Helper()
	b, err := voicebridge.New(g711, g711, time.Millisecond, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s, err := telnyx.NewBridgeSink(b, func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	s.SetClock(func() time.Time { return *now })
	s.Tap = tap
	return s
}

func ulawChunk(n int) []byte {
	c := make([]byte, n)
	for i := range c {
		c[i] = audio.LinearToUlaw(int16(i * 40))
	}
	return c
}

func TestAgentTapPlacesQueuedAudioAtQueueEnd(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	tap := &recTap{}
	s := newTapSink(t, tap, &now)
	ctx := context.Background()

	// 20 ms chunks: the first starts now; the second starts when the first ends.
	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "a", ContentIndex: 0}, ulawChunk(160)); err != nil {
		t.Fatal(err)
	}
	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "a", ContentIndex: 1}, ulawChunk(160)); err != nil {
		t.Fatal(err)
	}
	if len(tap.agent) != 2 {
		t.Fatalf("tap saw %d agent chunks, want 2", len(tap.agent))
	}
	if !tap.agent[0].playStart.Equal(now) {
		t.Fatalf("first play start = %v, want now %v", tap.agent[0].playStart, now)
	}
	if want := now.Add(20 * time.Millisecond); !tap.agent[1].playStart.Equal(want) {
		t.Fatalf("queued play start = %v, want %v", tap.agent[1].playStart, want)
	}
}

func TestAgentTapNeverSeesGatedAudio(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	tap := &recTap{}
	s := newTapSink(t, tap, &now)
	ctx := context.Background()

	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "cut", ContentIndex: 0}, ulawChunk(160)); err != nil {
		t.Fatal(err)
	}
	// The interrupt closes the anonymous gate and marks the item cut.
	if _, err := s.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	// Audio for the cut item is gated: nothing is sent and the tap is not called.
	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "cut", ContentIndex: 1}, ulawChunk(160)); err != nil {
		t.Fatal(err)
	}
	if len(tap.agent) != 1 {
		t.Fatalf("gated audio reached the tap: %d chunks", len(tap.agent))
	}
	if len(tap.cuts) != 1 {
		t.Fatalf("cut not reported to the tap: %d", len(tap.cuts))
	}
}

func TestCallerTapReceivesRTPTimestampAndCutTime(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	tap := &recTap{}
	s := newTapSink(t, tap, &now)
	ctx := context.Background()

	frame := telnyx.Frame{Track: "inbound", Timestamp: 4242, Payload: ulawChunk(160)}
	if err := s.Media(frame); err != nil {
		t.Fatal(err)
	}
	if len(tap.caller) != 1 || tap.caller[0] != 4242 {
		t.Fatalf("caller tap = %v, want one frame at ts 4242", tap.caller)
	}
	now = now.Add(time.Second)
	if _, err := s.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if len(tap.cuts) != 1 || !tap.cuts[0].Equal(now) {
		t.Fatalf("cut time = %v, want %v", tap.cuts, now)
	}
}

// Audio the provider never received is not recorded as played: a failed send
// returns its error and the tap is not called for that chunk.
func TestAgentTapSkipsChunkWhenSendFails(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	tap := &recTap{}
	b, err := voicebridge.New(g711, g711, time.Millisecond, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fail := true
	s, err := telnyx.NewBridgeSink(b, func(context.Context, []byte) error {
		if fail {
			return errSendDown
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.SetClock(func() time.Time { return now })
	s.Tap = tap
	ctx := context.Background()

	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "a", ContentIndex: 0}, ulawChunk(160)); err == nil {
		t.Fatal("a failed send returned no error")
	}
	if len(tap.agent) != 0 {
		t.Fatalf("tap saw %d chunks the provider never received", len(tap.agent))
	}
	fail = false
	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "a", ContentIndex: 1}, ulawChunk(160)); err != nil {
		t.Fatal(err)
	}
	if len(tap.agent) != 1 {
		t.Fatalf("tap after a successful send = %d chunks, want 1", len(tap.agent))
	}
}

var errSendDown = errors.New("send down")
