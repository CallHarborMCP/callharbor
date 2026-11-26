package telnyx_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
)

// newGateSink builds a sink whose sends are recorded as event names.
func newGateSink(t *testing.T, now func() time.Time) (*telnyx.BridgeSink, *[]string) {
	t.Helper()
	b, err := voicebridge.New(g711, g711, time.Millisecond, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	s, err := telnyx.NewBridgeSink(b, func(_ context.Context, msg []byte) error {
		var m struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(msg, &m); err != nil {
			return err
		}
		events = append(events, m.Event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if now != nil {
		s.SetClock(now)
	}
	return s, &events
}

func gateChunk() []byte {
	c := make([]byte, 160)
	for i := range c {
		c[i] = audio.LinearToUlaw(int16(i * 50))
	}
	return c
}

// Audio without an item ID must not bypass a closed turn gate, across
// interleavings of caller frames, cuts and agent audio. The gate is closed by a
// cut and reopened by a caller speech end edge in Media (or by the turn bound).
// This asserts the current behaviour; it is not a record of a fixed defect.
func TestAnonymousAudioStaysGatedAcrossInterleavings(t *testing.T) {
	ctx := context.Background()
	chunk := gateChunk()
	cases := []struct {
		name string
		ops  []string // "anon", "item", "cut", "reopen", "cutitem"
		// wantDelivered is the number of media frames expected for the trailing "anon".
		wantLastAnonMedia bool
	}{
		{"anon after cut is dropped", []string{"anon", "cut", "anon"}, false},
		{"repeated cut keeps gate closed", []string{"cut", "anon", "cut", "anon"}, false},
		{"item audio does not reopen anon", []string{"cut", "item", "anon"}, false},
		{"reopen opens anon", []string{"cut", "reopen", "anon"}, true},
		{"anon before any cut plays", []string{"anon"}, true},
		{"reopen then cut closes again", []string{"reopen", "cut", "anon"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, events := newGateSink(t, nil)
			// mark the event count before the final operation; anything sent by
			// that operation is what the expectation is about.
			before := 0
			for i, op := range tc.ops {
				if i == len(tc.ops)-1 {
					before = len(*events)
				}
				switch op {
				case "anon":
					if err := s.AgentAudio(ctx, audio.SourceRef{}, chunk); err != nil {
						t.Fatal(err)
					}
				case "item":
					if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "fresh", ContentIndex: i}, chunk); err != nil {
						t.Fatal(err)
					}
				case "cut":
					if _, err := s.Interrupt(ctx); err != nil {
						t.Fatal(err)
					}
				case "reopen":
					s.ReopenTurn()
				}
			}
			delivered := countAfter(*events, before) > 0
			if delivered != tc.wantLastAnonMedia {
				t.Fatalf("trailing anonymous audio delivered=%v, want %v; events=%v", delivered, tc.wantLastAnonMedia, *events)
			}
		})
	}
}

// countAfter reports how many media frames were sent at or after index i.
func countAfter(events []string, i int) int {
	n := 0
	for _, e := range events[i:] {
		if e == "media" {
			n++
		}
	}
	return n
}

// Empty-ID audio after a cut is dropped; it is sent again only after ReopenTurn,
// and an identified item cut earlier stays suppressed.
func TestCutItemStaysSuppressedAfterReopen(t *testing.T) {
	ctx := context.Background()
	s, events := newGateSink(t, nil)
	chunk := gateChunk()
	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "cut-me"}, chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	s.ReopenTurn()
	before := len(*events)
	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "cut-me", ContentIndex: 1}, chunk); err != nil {
		t.Fatal(err)
	}
	if len(*events) != before {
		t.Fatal("cut item resumed after ReopenTurn")
	}
	if err := s.AgentAudio(ctx, audio.SourceRef{}, chunk); err != nil {
		t.Fatal(err)
	}
	if len(*events) == before {
		t.Fatal("anonymous audio stayed closed after ReopenTurn")
	}
}

// Interrupt reports what was cut: the item, its content index, the estimated
// played duration and whether audio was audible.
func TestInterruptReportsCutValues(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	clock := t0
	s, _ := newGateSink(t, func() time.Time { return clock })

	// 160 bytes of G.711 at 8 kHz is 20 ms.
	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "item-x", ContentIndex: 2}, gateChunk()); err != nil {
		t.Fatal(err)
	}
	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "item-x", ContentIndex: 2}, gateChunk()); err != nil {
		t.Fatal(err)
	}
	// The second chunk starts at t0+20ms (the first finishes then). At 30 ms the
	// first chunk has fully played, though no echo has arrived for it, and 10 ms
	// of the second is audible. The cut reports the cut-time position: the
	// finished-but-unechoed 20 ms plus 10 ms in flight, so 30 ms of 40 ms sent.
	// Reporting only echo-confirmed audio would have said 0 here.
	clock = t0.Add(30 * time.Millisecond)
	cut, err := s.Interrupt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cut.ItemID != "item-x" || cut.ContentIndex != 2 {
		t.Fatalf("cut identity = %+v", cut)
	}
	if !cut.Audible {
		t.Fatal("audio was playing at the cut")
	}
	if cut.Sent != 40*time.Millisecond {
		t.Fatalf("sent = %v, want 40ms", cut.Sent)
	}
	if cut.Played != 30*time.Millisecond {
		t.Fatalf("played = %v, want 30ms: 20ms finished (unechoed) plus 10ms in flight", cut.Played)
	}
}

// A cut with nothing playing reports Audible false, so no truncate is due.
func TestInterruptAfterPlayoutIsNotAudible(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	clock := t0
	s, _ := newGateSink(t, func() time.Time { return clock })
	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "done"}, gateChunk()); err != nil {
		t.Fatal(err)
	}
	clock = t0.Add(time.Second)
	cut, err := s.Interrupt(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cut.Audible {
		t.Fatalf("finished playout reported audible: %+v", cut)
	}
}

// Pending marks are capped. Once the cap is reached, further agent audio is
// refused with ErrPlayoutBackpressure and nothing is sent for it.
func TestPendingMarkCapIsEnforced(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	s, events := newGateSink(t, func() time.Time { return t0 })
	var lastErr error
	sent := 0
	for i := 0; i < telnyx.MaxPendingMarks+16; i++ {
		lastErr = s.AgentAudio(ctx, audio.SourceRef{ItemID: fmt.Sprintf("p-%d", i)}, gateChunk())
		if lastErr != nil {
			break
		}
		sent++
	}
	if lastErr == nil {
		t.Fatal("pending marks grew past the cap with no backpressure")
	}
	if want := telnyx.ErrPlayoutBackpressure; lastErr != want {
		t.Fatalf("err = %v, want ErrPlayoutBackpressure", lastErr)
	}
	if sent != telnyx.MaxPendingMarks {
		t.Fatalf("accepted %d chunks, want exactly the cap %d", sent, telnyx.MaxPendingMarks)
	}
	before := len(*events)
	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "over"}, gateChunk()); err != telnyx.ErrPlayoutBackpressure {
		t.Fatalf("over-cap err = %v", err)
	}
	if len(*events) != before {
		t.Fatal("over-cap audio reached Telnyx")
	}
}

// Expired marks do not count toward the cap. Marks that Telnyx never echoes
// expire after the ledger grace; their chunks are then no longer awaited, so
// the stream accepts agent audio again instead of staying refused.
func TestExpiredMarksDoNotCountTowardCap(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	clock := t0
	s, _ := newGateSink(t, func() time.Time { return clock })
	for i := 0; i < telnyx.MaxPendingMarks; i++ {
		if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: fmt.Sprintf("e-%d", i)}, gateChunk()); err != nil {
			t.Fatalf("chunk %d refused before the cap: %v", i, err)
		}
	}
	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "full"}, gateChunk()); err != telnyx.ErrPlayoutBackpressure {
		t.Fatalf("at the cap err = %v, want ErrPlayoutBackpressure", err)
	}
	// Every queued chunk finishes and its echo never comes: past the grace the
	// marks expire and no longer hold the cap.
	clock = t0.Add(time.Duration(telnyx.MaxPendingMarks)*20*time.Millisecond + time.Hour)
	if err := s.AgentAudio(ctx, audio.SourceRef{ItemID: "after"}, gateChunk()); err != nil {
		t.Fatalf("expired marks still hold the cap: %v", err)
	}
}
