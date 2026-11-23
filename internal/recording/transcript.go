// Package recording holds transcript provenance and fidelity rules for
// two-party call records.
package recording

import (
	"errors"
	"fmt"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
)

// Speaker identifies a call party.
type Speaker string

const (
	SpeakerCaller Speaker = "caller"
	SpeakerAgent  Speaker = "agent"
)

// Fidelity states how trustworthy a transcript entry is relative to what was
// actually heard on the call.
type Fidelity string

const (
	// FidelityPlayed: all agent audio for the text was confirmed played.
	FidelityPlayed Fidelity = "played"
	// FidelityInterrupted: playback was cut; Text is the provider's full
	// generated transcript and includes words that were not heard. The
	// audible boundary inside the text is unknown (no word timing upstream).
	FidelityInterrupted Fidelity = "interrupted_text_includes_unplayed"
	// FidelityUnconfirmed: playback could not be confirmed.
	FidelityUnconfirmed Fidelity = "playout_unconfirmed"
	// FidelityNotPlayed: none of the audio was heard; text must not be shown
	// as spoken.
	FidelityNotPlayed Fidelity = "not_played"
	// FidelityASR: caller speech recognised by the provider; approximate.
	FidelityASR Fidelity = "provider_asr_approximate"
	// FidelityUnavailable: no transcript exists for this speech.
	FidelityUnavailable Fidelity = "unavailable"
)

// Entry is a transcript line with provenance.
type Entry struct {
	Speaker  Speaker
	Text     string
	Fidelity Fidelity
	// SourceIDs are verbatim provider identifiers (item_id/event_id).
	SourceIDs []string
	// Generated/Played durations of agent audio for this entry.
	Generated time.Duration
	Played    time.Duration
	// PlayedIsEstimate is true when Played includes elapsed-time estimates
	// for audio interrupted by clear (no per-chunk confirmation).
	PlayedIsEstimate bool
	// Timing is provider-supplied session-relative timing when available.
	HasProviderTiming bool
	StartMS, EndMS    int64
}

var ErrNoTranscript = errors.New("recording: transcript text unavailable")

// AgentEntry reconciles a provider agent transcript with playout evidence.
// It never trims or invents words: the upstream protocols provide no word
// alignment, so interrupted text is flagged instead of cut.
func AgentEntry(text string, sourceIDs []string, p audio.ItemPlayout) (Entry, error) {
	if text == "" && p.Sent > 0 {
		return Entry{}, fmt.Errorf("%w: agent audio %v has no transcript", ErrNoTranscript, p.Sent)
	}
	e := Entry{Speaker: SpeakerAgent, Text: text, SourceIDs: sourceIDs, Generated: p.Sent}
	switch {
	case p.Interrupted:
		e.Played, e.PlayedIsEstimate = p.Estimated, true
		if p.Estimated == 0 {
			e.Fidelity = FidelityNotPlayed
		} else {
			e.Fidelity = FidelityInterrupted
		}
	case p.Unconfirmed > 0 || p.Confirmed < p.Sent:
		e.Played = p.Confirmed
		e.Fidelity = FidelityUnconfirmed
	default:
		e.Played = p.Confirmed
		e.Fidelity = FidelityPlayed
	}
	return e, nil
}

// CallerEntry records provider-recognised caller speech. A missing transcript
// yields FidelityUnavailable with empty text, never a guess.
func CallerEntry(text string, available bool, sourceIDs []string, hasTiming bool, startMS, endMS int64) Entry {
	e := Entry{Speaker: SpeakerCaller, SourceIDs: sourceIDs, HasProviderTiming: hasTiming, StartMS: startMS, EndMS: endMS}
	if !available {
		e.Fidelity = FidelityUnavailable
		return e
	}
	e.Text, e.Fidelity = text, FidelityASR
	return e
}

// Complete reports whether entries form a truthful full two-party transcript:
// every entry has text, agent audio was played or honestly flagged, and both
// parties are present.
func Complete(entries []Entry) error {
	var caller, agent bool
	for i, e := range entries {
		switch e.Fidelity {
		case FidelityUnavailable:
			return fmt.Errorf("%w: entry %d (%s)", ErrNoTranscript, i, e.Speaker)
		case FidelityUnconfirmed:
			return fmt.Errorf("recording: entry %d agent playout unconfirmed", i)
		}
		if e.Speaker == SpeakerCaller {
			caller = true
		}
		if e.Speaker == SpeakerAgent && e.Fidelity != FidelityNotPlayed {
			agent = true
		}
	}
	if !caller || !agent {
		return fmt.Errorf("%w: caller=%v agent=%v", ErrNoTranscript, caller, agent)
	}
	return nil
}
