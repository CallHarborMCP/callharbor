package telnyx_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
)

// Caller-turn tests for the sink. They use the real BridgeSink and a real
// voicebridge.Bridge, with caller frames that drive the local detector through
// Media. The clock is injected, so every decision is deterministic.

// turnTone returns one 20 ms loud 440 Hz frame, well above the detector's
// threshold. turnSilence is G.711 silence.
func turnTone() []byte {
	c := make([]byte, 160)
	for i := range c {
		c[i] = audio.LinearToUlaw(int16(8000 * math.Sin(2*math.Pi*440*float64(i)/8000)))
	}
	return c
}

func turnSilence() []byte { return bytes.Repeat([]byte{0xFF}, 160) }

type turnHarness struct {
	t     *testing.T
	sink  *telnyx.BridgeSink
	now   time.Time
	sent  []string // event names sent to Telnyx, in order
	edges []telnyx.SpeechEvent
	seq   int
}

func newTurnHarness(t *testing.T) *turnHarness {
	t.Helper()
	h := &turnHarness{t: t, now: time.Unix(1_800_000_000, 0)}
	vb, err := voicebridge.New(g711, g711, time.Millisecond, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s, err := telnyx.NewBridgeSink(vb, func(_ context.Context, msg []byte) error {
		var m struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(msg, &m); err != nil {
			return err
		}
		h.sent = append(h.sent, m.Event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.SetClock(func() time.Time { return h.now })
	s.OnCallerSpeech = func(ev telnyx.SpeechEvent) { h.edges = append(h.edges, ev) }
	s.ToProvider = func([]byte) error { return nil }
	h.sink = s
	return h
}

// frame delivers one 20 ms caller frame through Media.
func (h *turnHarness) frame(payload []byte) {
	h.t.Helper()
	h.seq++
	b, _ := json.Marshal(map[string]any{
		"event": "media", "sequence_number": fmt.Sprint(h.seq), "stream_id": "s1",
		"media": map[string]any{"track": "inbound", "chunk": fmt.Sprint(h.seq), "timestamp": fmt.Sprint(h.seq * 20), "payload": base64.StdEncoding.EncodeToString(payload)},
	})
	f, err := telnyx.ParseFrame(b)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.sink.Media(f); err != nil {
		h.t.Fatal(err)
	}
	h.now = h.now.Add(20 * time.Millisecond)
}

// speak sends a caller onset (enough tone frames for a Start edge).
func (h *turnHarness) speak() {
	for i := 0; i < 5; i++ {
		h.frame(turnTone())
	}
}

// hang sends enough silence for an End edge.
func (h *turnHarness) hang() {
	for i := 0; i < 25; i++ {
		h.frame(turnSilence())
	}
}

// agent queues one reply with an empty item ID: 40 chunks, 800 ms of audio,
// long enough to still be playing across a caller onset.
func (h *turnHarness) agent(chunk []byte) {
	h.t.Helper()
	for i := 0; i < 40; i++ {
		if err := h.sink.AgentAudio(context.Background(), audio.SourceRef{}, chunk); err != nil {
			h.t.Fatal(err)
		}
	}
}

// startEdge returns the most recent Start edge recorded.
func (h *turnHarness) startEdge() telnyx.SpeechEvent {
	h.t.Helper()
	for i := len(h.edges) - 1; i >= 0; i-- {
		if h.edges[i].Started {
			return h.edges[i]
		}
	}
	h.t.Fatal("no Start edge was delivered")
	return telnyx.SpeechEvent{}
}

func (h *turnHarness) countSent(event string) int {
	n := 0
	for _, e := range h.sent {
		if e == event {
			n++
		}
	}
	return n
}

// Regression: a Start edge delivered while the agent was audible, whose caller
// turn has since ended and whose successor reply is now queued, must not cut the
// new reply. The engine may process the stale Start late; the decision is made
// by the sink against the caller's current state, so it is refused.
func TestStaleStartAfterEndDoesNotCutNewReply(t *testing.T) {
	h := newTurnHarness(t)

	// Reply 0 is playing. The caller starts speaking over it.
	h.agent(turnTone())
	h.speak()
	start := h.startEdge()
	if !start.Audible {
		t.Fatal("start edge over a playing reply must be marked audible")
	}

	// The caller stops before the engine reads the Start: End, then reopen.
	h.hang()

	// A new reply is queued after the End edge.
	h.agent(turnTone())

	// The engine now processes the stale Start.
	_, ok, err := h.sink.InterruptTurn(context.Background(), start.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("stale start cut a reply that began after the caller stopped")
	}
	if n := h.countSent("clear"); n != 0 {
		t.Fatalf("stale start sent %d clear frames, want none", n)
	}
}

// A genuine barge-in: agent audible and the caller still in the turn. The cut
// is allowed and sends one clear.
func TestBargeInWhileCallerSpeaksCuts(t *testing.T) {
	h := newTurnHarness(t)
	h.agent(turnTone())
	h.speak()
	start := h.startEdge()
	if !start.Audible {
		t.Fatal("start edge over a playing reply must be marked audible")
	}
	_, ok, err := h.sink.InterruptTurn(context.Background(), start.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("barge-in while the caller is still speaking was refused")
	}
	if n := h.countSent("clear"); n != 1 {
		t.Fatalf("barge-in sent %d clear frames, want 1", n)
	}
}

// An onset while the agent is silent is an ordinary turn: the Start edge is not
// marked audible, and no cut is requested.
func TestOnsetWhileAgentSilentIsNotAudible(t *testing.T) {
	h := newTurnHarness(t)
	h.speak()
	start := h.startEdge()
	if start.Audible {
		t.Fatal("start edge with nothing playing was marked audible")
	}
}

// A Start edge that is dropped before the engine reads it must not cause a cut
// later. Dropping is handled by the queue in voicecall; at the sink the same
// guarantee holds: a turn that ended is never cut by a later call.
func TestDroppedStartNeverCutsLater(t *testing.T) {
	h := newTurnHarness(t)
	h.agent(turnTone())
	h.speak()
	start := h.startEdge()
	// The Start edge is never delivered to the engine (dropped). The caller
	// stops, and a new reply plays.
	h.hang()
	h.agent(turnTone())
	_, ok, err := h.sink.InterruptTurn(context.Background(), start.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if ok || h.countSent("clear") != 0 {
		t.Fatal("a dropped start that ended later cut a new reply")
	}
}

// A second turn's Start must not be cut by the first turn's number.
func TestInterruptTurnRejectsOlderTurn(t *testing.T) {
	h := newTurnHarness(t)
	h.agent(turnTone())
	h.speak()
	first := h.startEdge()
	h.hang()
	h.speak()
	if h.startEdge().Turn == first.Turn {
		t.Fatal("two caller turns share a turn number")
	}
	_, ok, err := h.sink.InterruptTurn(context.Background(), first.Turn)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("an older turn number cut the current turn")
	}
}
