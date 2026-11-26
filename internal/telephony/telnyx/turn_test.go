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

// Stuck line noise: the detector stays in a turn that never ends, so no End
// edge arrives. The anonymous gate must not stay closed past MaxCallerTurn, or
// the agent would be mute until max_duration. Once the bound passes, the next
// caller frame force-ends the turn and reopens the gate.
func TestStuckCallerTurnIsForceEndedAndReopensGate(t *testing.T) {
	h := newTurnHarness(t)
	h.agent(turnTone())
	h.speak()
	start := h.startEdge()
	// Close the anonymous gate with a real cut while the turn is open.
	if _, ok, err := h.sink.InterruptTurn(context.Background(), start.Turn); err != nil || !ok {
		t.Fatalf("barge-in on the open turn was refused: ok=%v err=%v", ok, err)
	}
	before := h.countSent("media")
	h.agent(turnTone())
	if n := h.countSent("media") - before; n != 0 {
		// The reply queued after the cut must be gated while the turn is open.
		t.Fatalf("gated reply reached Telnyx: %d media frames", n)
	}

	// Loud frames keep arriving for longer than the bound with no silence.
	for h.now.Sub(start.At) < telnyx.MaxCallerTurn {
		h.frame(turnTone())
	}
	if h.countEdges(false) != 0 {
		t.Fatal("the turn ended before the bound; the test premise is wrong")
	}
	h.frame(turnTone())
	if h.countEdges(false) != 1 {
		t.Fatalf("stuck turn was not force-ended: end edges=%d", h.countEdges(false))
	}
	// The gate is open again: the next reply plays.
	before = h.countSent("media")
	h.agent(turnTone())
	if n := h.countSent("media") - before; n == 0 {
		t.Fatal("reply after the forced turn end was still gated")
	}
}

// The forced end must reset the local detector. A detector left speaking by a
// stuck turn would emit no Start edge for the caller's continuing speech, so
// barge-in and turn reopening would never see it, and silence accounting would
// count from the forced end while the caller is still talking. Loud audio that
// continues after the forced end must therefore yield a fresh Start edge with a
// new turn number. The noise is not stopped with silence, because the
// detector's own hangover would hide the defect.
func TestForcedTurnEndResetsDetectorSoContinuedSpeechStarts(t *testing.T) {
	h := newTurnHarness(t)
	h.speak()
	first := h.startEdge()
	for h.now.Sub(first.At) < telnyx.MaxCallerTurn {
		h.frame(turnTone())
	}
	// Still loud: the forced end fires on this frame.
	h.frame(turnTone())
	if h.countEdges(false) != 1 {
		t.Fatalf("stuck turn was not force-ended: end edges=%d", h.countEdges(false))
	}
	startsBefore := h.countEdges(true)
	// The caller keeps speaking after the forced end. Ample loud frames are
	// sent, enough for a fresh onset to be judged.
	for i := 0; i < 10; i++ {
		h.frame(turnTone())
	}
	if h.countEdges(true) == startsBefore {
		t.Fatal("continued speech after the forced end produced no Start edge: detector still speaking")
	}
	if got := h.startEdge(); got.Turn == first.Turn {
		t.Fatalf("continued speech reused turn %d", got.Turn)
	}
}

// countEdges counts delivered edges of one kind.
func (h *turnHarness) countEdges(started bool) int {
	n := 0
	for _, e := range h.edges {
		if e.Started == started {
			n++
		}
	}
	return n
}

// The anonymous gate reopens on a real caller speech end edge, through Media,
// and not before it: a cut with no end edge keeps empty-ID audio gated.
func TestAnonymousGateReopensOnlyOnEndEdge(t *testing.T) {
	h := newTurnHarness(t)
	h.agent(turnTone())
	h.speak()
	start := h.startEdge()
	if _, ok, err := h.sink.InterruptTurn(context.Background(), start.Turn); err != nil || !ok {
		t.Fatalf("barge-in refused: ok=%v err=%v", ok, err)
	}
	before := h.countSent("media")
	h.agent(turnTone())
	if n := h.countSent("media") - before; n != 0 {
		t.Fatalf("empty-ID audio passed the closed gate: %d frames", n)
	}
	h.hang() // real caller end edge
	before = h.countSent("media")
	h.agent(turnTone())
	if n := h.countSent("media") - before; n == 0 {
		t.Fatal("empty-ID audio still gated after the caller's end edge")
	}
}

// Documented clipping on live_ws: a provider reply that is sent before the
// caller's local end edge is dropped while the anonymous gate is closed. The
// test fixes the order of the reply and the end edge, so it is deterministic.
// This is the documented behaviour and a live-unverified risk, not a desired
// one; FEASIBILITY.md records it as a release blocker.
func TestEarlyReplyBeforeEndEdgeIsClipped(t *testing.T) {
	h := newTurnHarness(t)
	h.agent(turnTone())
	h.speak()
	start := h.startEdge()
	if _, ok, err := h.sink.InterruptTurn(context.Background(), start.Turn); err != nil || !ok {
		t.Fatalf("barge-in refused: ok=%v err=%v", ok, err)
	}
	// The caller is still in turn (no end edge yet). The provider's reply to the
	// cut turn arrives now, before the local end edge, and is gated out.
	before := h.countSent("media")
	h.agent(turnTone())
	if n := h.countSent("media") - before; n != 0 {
		t.Fatalf("early reply reached Telnyx while the gate was closed: %d frames", n)
	}
	// The end edge arrives. Audio sent after it plays.
	h.hang()
	before = h.countSent("media")
	h.agent(turnTone())
	if n := h.countSent("media") - before; n == 0 {
		t.Fatal("reply after the end edge was still gated")
	}
}
