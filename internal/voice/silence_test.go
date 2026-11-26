package voice

import (
	"testing"
	"time"
)

// fixedPlayout is a PlayoutClock whose queued-audio end the test sets directly.
type fixedPlayout struct{ end time.Time }

func (p *fixedPlayout) PlayoutEnd() time.Time { return p.end }

// The silence window must not start while agent audio is still playing, and it
// must start from the moment that audio finished, not from the caller's last
// edge. A long reply followed by a short pause is not silence long enough to
// end the call; the same pause measured from the caller's edge would be.
func TestSilenceMeasuresFromLastPlayoutEnd(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	const timeout = 30 * time.Second
	p := &fixedPlayout{}
	act := newActivity(t0, p)

	// The caller stops at t0 and the agent replies for 25 s.
	act.onSpeech(SpeechEvent{Started: false, At: t0})
	p.end = t0.Add(25 * time.Second)

	// While the reply plays, the caller is not silent at all.
	if got := act.silentFor(t0.Add(10 * time.Second)); got != 0 {
		t.Fatalf("silent while agent audio plays: %v", got)
	}

	// Reply finished at t0+25s. The caller pauses 5 s. The silence since the
	// reply ended is 5 s, far short of the 30 s timeout. Measured from the
	// caller's edge it would be 30 s and end the call wrongly.
	if got, want := act.silentFor(t0.Add(30*time.Second)), 5*time.Second; got != want {
		t.Fatalf("silence after a 25 s reply and 5 s pause = %v, want %v", got, want)
	}
	if act.silentFor(t0.Add(30*time.Second)) >= timeout {
		t.Fatal("a 5 s pause after a long reply ended the call as caller silence")
	}

	// Silence that runs past the timeout after the reply does end the call.
	if got := act.silentFor(t0.Add(25*time.Second + timeout)); got < timeout {
		t.Fatalf("silence of %v after the reply did not reach the timeout", got)
	}
}

// The caller's own edge still starts the window when no agent audio plays.
func TestSilenceMeasuresFromCallerEdgeWhenAgentSilent(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	p := &fixedPlayout{end: t0.Add(-time.Second)}
	act := newActivity(t0, p)
	act.onSpeech(SpeechEvent{Started: false, At: t0.Add(2 * time.Second)})
	if got, want := act.silentFor(t0.Add(5*time.Second)), 3*time.Second; got != want {
		t.Fatalf("silence = %v, want %v", got, want)
	}
}

// A reply that starts and finishes entirely between two watchdog polls is
// still agent activity. The watchdog must not forget it: the silence window
// starts when that reply finished, even though no poll saw it playing.
func TestSilenceSurvivesReplyBetweenPolls(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	p := &fixedPlayout{end: t0.Add(-time.Second)}
	act := newActivity(t0, p)
	// The caller stops at t0. A 10 s reply plays and finishes before the next
	// poll, which is the first time the watchdog observes PlayoutEnd.
	act.onSpeech(SpeechEvent{Started: false, At: t0})
	p.end = t0.Add(10 * time.Second)
	// The first poll is after the reply finished, with a 5 s pause since then.
	if got, want := act.silentFor(t0.Add(15*time.Second)), 5*time.Second; got != want {
		t.Fatalf("silence after an unpolled 10 s reply and 5 s pause = %v, want %v", got, want)
	}
}
