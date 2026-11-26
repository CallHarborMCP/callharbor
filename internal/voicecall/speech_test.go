package voicecall

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/voice"
)

// newSpeechBridge builds an AudioBridge whose speech channel is not drained by
// anything, so the tests control exactly what is queued.
func newSpeechBridge(t *testing.T) *AudioBridge {
	t.Helper()
	return &AudioBridge{speech: make(chan voice.SpeechEvent, speechQueue), done: make(chan struct{})}
}

// drainSpeech returns the queued edges in order without blocking.
func drainSpeech(b *AudioBridge) []voice.SpeechEvent {
	var out []voice.SpeechEvent
	for {
		select {
		case ev := <-b.CallerSpeech():
			out = append(out, ev)
		default:
			return out
		}
	}
}

// TestCallerSpeechIsBounded checks that the queue never holds more than
// speechQueue edges, however many arrive while the engine is not reading.
func TestCallerSpeechIsBounded(t *testing.T) {
	b := newSpeechBridge(t)
	t0 := time.Unix(1_800_000_000, 0)
	for i := 0; i < 5*speechQueue; i++ {
		b.enqueueSpeech(telnyx.SpeechEvent{Started: i%2 == 0, At: t0.Add(time.Duration(i) * time.Millisecond)})
		if n := len(b.speech); n > speechQueue {
			t.Fatalf("queue holds %d edges, bound is %d", n, speechQueue)
		}
	}
	if n := len(drainSpeech(b)); n != speechQueue {
		t.Fatalf("drained %d edges, want the full bound %d", n, speechQueue)
	}
}

// TestCallerSpeechDropsOldestWhenFull checks that when the queue is full, the
// oldest edges are the ones dropped, so the newest state is delivered.
func TestCallerSpeechDropsOldestWhenFull(t *testing.T) {
	b := newSpeechBridge(t)
	t0 := time.Unix(1_800_000_000, 0)
	// Fill with start edges, then add end edges: each end must displace the oldest.
	for i := 0; i < speechQueue; i++ {
		b.enqueueSpeech(telnyx.SpeechEvent{Started: true, At: t0.Add(time.Duration(i) * time.Millisecond)})
	}
	last := t0.Add(100 * time.Millisecond)
	b.enqueueSpeech(telnyx.SpeechEvent{Started: false, At: last})

	got := drainSpeech(b)
	if len(got) != speechQueue {
		t.Fatalf("queue holds %d edges, want %d", len(got), speechQueue)
	}
	if !got[len(got)-1].At.Equal(last) || got[len(got)-1].Started {
		t.Fatalf("newest edge is %+v, want the end edge at %v", got[len(got)-1], last)
	}
	// The oldest start edge (t0) must be the one dropped.
	if got[0].At.Equal(t0) {
		t.Fatal("oldest edge was kept; drop-oldest policy not applied")
	}
}

// TestCallerSpeechEndEdgeIsNeverLost checks that a full queue still accepts an
// end edge, since turn state depends on it, while a start edge is dropped.
func TestCallerSpeechEndEdgeIsNeverLost(t *testing.T) {
	b := newSpeechBridge(t)
	t0 := time.Unix(1_800_000_000, 0)
	for i := 0; i < speechQueue; i++ {
		b.enqueueSpeech(telnyx.SpeechEvent{Started: true, At: t0})
	}
	b.enqueueSpeech(telnyx.SpeechEvent{Started: true, At: t0.Add(time.Second)})
	for _, ev := range drainSpeech(b) {
		if ev.Started && ev.At.Equal(t0.Add(time.Second)) {
			t.Fatal("a start edge was queued when the queue was full; it must be dropped, not displace older state")
		}
	}

	b.enqueueSpeech(telnyx.SpeechEvent{Started: true, At: t0})
	for i := 0; i < speechQueue; i++ {
		b.enqueueSpeech(telnyx.SpeechEvent{Started: true, At: t0})
	}
	end := t0.Add(2 * time.Second)
	b.enqueueSpeech(telnyx.SpeechEvent{Started: false, At: end})
	got := drainSpeech(b)
	if len(got) == 0 || got[len(got)-1].Started || !got[len(got)-1].At.Equal(end) {
		t.Fatalf("end edge lost on a full queue; newest queued is %+v", got)
	}
}

// TestCallerSpeechNeverBlocks checks that enqueueing on a full queue returns
// promptly, for both start and end edges, with no reader present.
func TestCallerSpeechNeverBlocks(t *testing.T) {
	b := newSpeechBridge(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10*speechQueue; i++ {
			b.enqueueSpeech(telnyx.SpeechEvent{Started: i%3 != 0, At: time.Unix(1_800_000_000, 0)})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("enqueueSpeech blocked on a full queue with no reader")
	}
}

// TestReopenTurnReopensAnonymousGate checks the production reopen through the
// voicecall bridge. A cut closes the anonymous gate, so empty-ID agent audio is
// dropped. ReopenTurn must let the next empty-ID chunk reach Telnyx.
func TestReopenTurnReopensAnonymousGate(t *testing.T) {
	ctx := context.Background()
	ab, rec := newBridge(t)
	chunk := bytes.Repeat([]byte{0x7F}, 160)

	if _, err := ab.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	before := rec.mediaCount()
	if err := ab.Play(ctx, chunk); err != nil {
		t.Fatalf("Play while gated: %v", err)
	}
	if got := rec.mediaCount(); got != before {
		t.Fatalf("empty-ID audio reached Telnyx while the anonymous gate was closed (%d -> %d)", before, got)
	}

	ab.ReopenTurn()
	if err := ab.Play(ctx, chunk); err != nil {
		t.Fatalf("Play after ReopenTurn: %v", err)
	}
	if got := rec.mediaCount(); got != before+1 {
		t.Fatalf("empty-ID audio after ReopenTurn: media frames %d -> %d, want one more", before, got)
	}
}

// mediaCount is the number of media frames the recorder has seen.
func (r *recorder) mediaCount() int {
	n := 0
	for _, e := range r.events() {
		if e == "media" {
			n++
		}
	}
	return n
}
