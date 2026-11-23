package recording

import (
	"errors"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
)

func TestAgentEntryFidelity(t *testing.T) {
	ms := time.Millisecond
	cases := []struct {
		name string
		p    audio.ItemPlayout
		want Fidelity
	}{
		{"played", audio.ItemPlayout{Sent: 800 * ms, Confirmed: 800 * ms, Estimated: 800 * ms}, FidelityPlayed},
		{"interrupted", audio.ItemPlayout{Sent: 800 * ms, Confirmed: 200 * ms, Estimated: 250 * ms, Discarded: 550 * ms, Interrupted: true}, FidelityInterrupted},
		{"cut before audible", audio.ItemPlayout{Sent: 800 * ms, Discarded: 800 * ms, Interrupted: true}, FidelityNotPlayed},
		{"unconfirmed", audio.ItemPlayout{Sent: 800 * ms, Confirmed: 600 * ms, Unconfirmed: 200 * ms}, FidelityUnconfirmed},
		{"still pending", audio.ItemPlayout{Sent: 800 * ms, Confirmed: 600 * ms}, FidelityUnconfirmed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const text = "Your table for two is booked for seven."
			e, err := AgentEntry(text, []string{"item_1"}, tc.p)
			if err != nil {
				t.Fatal(err)
			}
			if e.Fidelity != tc.want {
				t.Fatalf("fidelity %s, want %s", e.Fidelity, tc.want)
			}
			if e.Text != text {
				t.Fatal("transcript text must never be trimmed or rewritten")
			}
			if e.Played > e.Generated {
				t.Fatalf("played %v > generated %v", e.Played, e.Generated)
			}
		})
	}
}

func TestNoSynthesizedTranscript(t *testing.T) {
	if _, err := AgentEntry("", nil, audio.ItemPlayout{Sent: time.Second}); !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("agent audio without transcript must fail: %v", err)
	}
	c := CallerEntry("I think they said yes", false, []string{"item_2"}, false, 0, 0)
	if c.Text != "" || c.Fidelity != FidelityUnavailable {
		t.Fatalf("unavailable caller transcript must be empty: %+v", c)
	}
	agent, _ := AgentEntry("Hello", nil, audio.ItemPlayout{Sent: time.Second, Confirmed: time.Second})
	if err := Complete([]Entry{agent, c}); !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("missing caller speech must fail completeness: %v", err)
	}
	if err := Complete([]Entry{agent}); !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("one-party transcript must fail completeness: %v", err)
	}
	notPlayed, _ := AgentEntry("Hello", nil, audio.ItemPlayout{Sent: time.Second, Discarded: time.Second, Interrupted: true})
	caller := CallerEntry("Hi", true, nil, true, 0, 10)
	if err := Complete([]Entry{notPlayed, caller}); err == nil {
		t.Fatal("unheard agent text must not count as agent speech")
	}
	if err := Complete([]Entry{agent, caller}); err != nil {
		t.Fatal(err)
	}
}
