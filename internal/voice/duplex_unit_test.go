package voice

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/realtime"
)

// truncSession is a fakeSession that also implements Truncater and records
// the truncate call.
type truncSession struct {
	*fakeSession
	mu        sync.Mutex
	truncates []string
}

func (s *truncSession) Truncate(_ context.Context, item string, idx int, ms int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.truncates = append(s.truncates, item)
	return nil
}

func (s *truncSession) truncated() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.truncates...)
}

// cutBridge returns a fixed Cut from Interrupt and counts interrupts.
type cutBridge struct {
	*fakeBridge
	cut Cut
}

func (b *cutBridge) Interrupt(ctx context.Context) (Cut, error) {
	_, _ = b.fakeBridge.Interrupt(ctx)
	return b.cut, nil
}

// counts returns the played bytes and interrupt count recorded so far.
func (b *fakeBridge) counts() (played, interrupts int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range b.played {
		played += len(p)
	}
	return played, b.barges
}

// Truncate decision: no truncate without an item, when nothing was audible,
// when fully played, or when the protocol cannot truncate.
func TestTruncateDecision(t *testing.T) {
	cases := []struct {
		name string
		cut  Cut
		sess realtime.Session
		want bool
	}{
		{"partially played item truncates", Cut{ItemID: "i", Played: 300 * time.Millisecond, Sent: time.Second, Audible: true}, &truncSession{fakeSession: newFakeSession()}, true},
		{"fully played item does not", Cut{ItemID: "i", Played: time.Second, Sent: time.Second, Audible: true}, &truncSession{fakeSession: newFakeSession()}, false},
		{"no item id does not", Cut{Played: 0, Sent: time.Second, Audible: true}, &truncSession{fakeSession: newFakeSession()}, false},
		{"nothing audible does not", Cut{ItemID: "i", Played: 0, Sent: time.Second, Audible: false}, &truncSession{fakeSession: newFakeSession()}, false},
		{"live session cannot truncate", Cut{ItemID: "i", Played: 300 * time.Millisecond, Sent: time.Second, Audible: true}, newFakeSession(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := truncateCut(context.Background(), tc.sess, tc.cut); err != nil {
				t.Fatal(err)
			}
			ts, ok := tc.sess.(*truncSession)
			got := ok && len(ts.truncated()) == 1
			if got != tc.want {
				t.Fatalf("truncated=%v, want %v", got, tc.want)
			}
		})
	}
}

// The truncate value is floor(played) in milliseconds and never above sent.
func TestTruncateReportsFlooredPlayedMilliseconds(t *testing.T) {
	var got int64 = -1
	sess := &recordTruncate{fakeSession: newFakeSession(), got: &got}
	cut := Cut{ItemID: "i", Played: 1234567 * time.Microsecond, Sent: 2 * time.Second, Audible: true}
	if err := truncateCut(context.Background(), sess, cut); err != nil {
		t.Fatal(err)
	}
	if got != 1234 {
		t.Fatalf("audio_end_ms = %d, want floor(1234.567ms)=1234", got)
	}
	if got > cut.Sent.Milliseconds() {
		t.Fatalf("audio_end_ms %d exceeds sent %d", got, cut.Sent.Milliseconds())
	}
}

type recordTruncate struct {
	*fakeSession
	got *int64
}

func (r *recordTruncate) Truncate(_ context.Context, _ string, _ int, ms int64) error {
	*r.got = ms
	return nil
}

// Realtime barge-in: provider speech_started cuts, and the truncate goes to the
// session with the cut item.
func TestSpeechStartedTruncatesInterruptedItem(t *testing.T) {
	sess := &truncSession{fakeSession: newFakeSession()}
	br := &cutBridge{fakeBridge: newFakeBridge(), cut: Cut{ItemID: "item_9", ContentIndex: 0, Played: 200 * time.Millisecond, Sent: time.Second, Audible: true}}
	sess.events <- realtime.Event{Kind: realtime.EventSpeechStarted}
	sess.events <- realtime.Event{Kind: realtime.EventClosed}
	if _, err := Run(context.Background(), sess, br, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sess.truncated(); len(got) != 1 || got[0] != "item_9" {
		t.Fatalf("truncates = %v, want [item_9]", got)
	}
}

// Live (no native speech start): a local onset while agent audio is audible
// cuts; an onset while the agent is silent does not.
type speechBridge struct {
	*cutBridge
	speech    chan SpeechEvent
	playUntil time.Time
	now       func() time.Time
	polls     atomic.Int64
}

func (b *speechBridge) CallerSpeech() <-chan SpeechEvent { return b.speech }
func (b *speechBridge) PlayoutEnd() time.Time {
	b.polls.Add(1)
	return b.playUntil
}

// watchStarted reports whether the engine has begun polling silence, which
// happens only after RunWith has taken its start time.
func watchStarted(b *speechBridge) bool { return b.polls.Load() > 0 }

func TestLiveLocalOnsetOnlyCutsWhileAgentIsAudible(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	now := func() time.Time { return t0 }

	// Agent silent: the transport marks the onset not audible, so the engine
	// requests no cut at all.
	br := newTurnBridge(t0, t0.Add(-time.Second))
	runSpeech(t, br, SpeechEvent{Started: true, At: t0, Turn: 1, Audible: false}, now)
	if _, interrupts := br.counts(); interrupts != 0 {
		t.Fatalf("onset while agent silent cut audio %d times", interrupts)
	}
	if got := br.turnCuts(); got != 0 {
		t.Fatalf("onset while agent silent requested %d turn cuts", got)
	}

	// Agent audible and the caller still in turn: one cut.
	br2 := newTurnBridge(t0, t0.Add(time.Second))
	runSpeech(t, br2, SpeechEvent{Started: true, At: t0, Turn: 1, Audible: true}, now)
	if _, interrupts := br2.counts(); interrupts != 1 {
		t.Fatalf("onset while agent audible cut %d times, want 1", interrupts)
	}
}

// waitFor polls a condition with a bounded deadline; it is a condition wait,
// not a sleep-as-synchronisation.
func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

// Silence watchdog with an injectable clock: after SilenceTimeout with no
// speech and no playout, the run ends with ErrCallerSilence and the session is
// closed (Live gets the documented close).
func TestSilenceWatchdogEndsRunWithSentinel(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := t0
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }

	sess := newFakeSession()
	br := &speechBridge{cutBridge: &cutBridge{fakeBridge: newFakeBridge()}, speech: make(chan SpeechEvent, 8), now: now}
	res := make(chan error, 1)
	go func() {
		// Native speech start: this test covers silence accounting only, so the
		// local barge-in setup check (which needs a TurnInterrupter) does not apply.
		_, err := RunWith(context.Background(), sess, br, nil, Options{Caps: realtime.RealtimeCapabilities, SilenceTimeout: 30 * time.Second, Now: now})
		res <- err
	}()
	// The silence window starts when the run starts. Advance the injected clock
	// past the timeout only after the run has begun, so the watchdog observes the
	// elapsed silence on its own tick. The run's start is taken from the clock
	// under the same lock, so this ordering is exact.
	waitFor(t, func() bool { return watchStarted(br) })
	advance(2 * time.Minute)
	select {
	case err := <-res:
		if !errors.Is(err, ErrCallerSilence) {
			t.Fatalf("err = %v, want ErrCallerSilence", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("silence watchdog never fired")
	}
	select {
	case <-sess.closed:
	default:
		t.Fatal("session not closed on silence stop")
	}
}

// Negative silence timeout is refused; zero disables the watchdog.
func TestSilenceOptionsValidation(t *testing.T) {
	if _, err := RunWith(context.Background(), newFakeSession(), newFakeBridge(), nil, Options{SilenceTimeout: -time.Second}); err == nil {
		t.Fatal("negative silence timeout accepted")
	}
}

// The stale-onset regression lives at the transport: see
// internal/telephony/telnyx/turn_test.go (TestStaleStartAfterEndDoesNotCutNewReply).

// turnBridge is a speech bridge whose cut honours the caller turn, as the
// transport does. It counts turn-scoped cut requests and the cuts they make.
type turnBridge struct {
	*speechBridge
	mu        sync.Mutex
	open      uint64 // turn currently open at the transport, 0 if none
	requested int
}

func newTurnBridge(now, playUntil time.Time) *turnBridge {
	return &turnBridge{speechBridge: &speechBridge{
		cutBridge: &cutBridge{fakeBridge: newFakeBridge(), cut: Cut{ItemID: "", Sent: time.Second, Audible: true}},
		speech:    make(chan SpeechEvent, 8),
		now:       func() time.Time { return now },
		playUntil: playUntil,
	}}
}

// InterruptTurn cuts only while the named turn is still open.
func (b *turnBridge) InterruptTurn(ctx context.Context, turn uint64) (Cut, bool, error) {
	b.mu.Lock()
	b.requested++
	open := b.open == turn
	b.mu.Unlock()
	if !open {
		return Cut{}, false, nil
	}
	c, err := b.Interrupt(ctx)
	return c, true, err
}

func (b *turnBridge) turnCuts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requested
}

// runSpeech delivers one edge to the engine and waits until it has been
// consumed, then stops the run. The edge's turn is opened on the transport so
// a Start that is still current can be cut.
func runSpeech(t *testing.T, br *turnBridge, ev SpeechEvent, now func() time.Time) {
	t.Helper()
	if ev.Started {
		br.mu.Lock()
		br.open = ev.Turn
		br.mu.Unlock()
	}
	sess := newFakeSession()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res := make(chan error, 1)
	go func() {
		_, err := RunWith(ctx, sess, br, nil, Options{Caps: realtime.LiveCapabilities, Now: now})
		res <- err
	}()
	br.speech <- ev
	// Wait until the engine has consumed the edge. An audible onset is done once
	// it has requested its cut; anything else is done once the channel drains.
	// These are condition checks, not sleeps standing in for synchronisation.
	if ev.Started && ev.Audible {
		waitFor(t, func() bool { return br.turnCuts() == 1 })
	} else {
		waitFor(t, func() bool { return len(br.speech) == 0 })
	}
	cancel()
	if err := <-res; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run ended with %v, want cancellation", err)
	}
}
