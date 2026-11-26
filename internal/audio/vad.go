package audio

import (
	"fmt"
	"math"
	"time"
)

// Local caller-speech detection. It is provider-neutral: it sees linear
// samples and a sample clock, never a provider event, so it works for any
// protocol. It is a plain RMS energy gate with an onset window and a hangover.
// Its thresholds are documented constants, not measured on live lines: line
// noise, echo and background speech can produce false onsets, which is an
// accepted limitation recorded in docs/protocol/FEASIBILITY.md.

const (
	// VADFrame is the analysis window. Frames are counted in media time, so
	// the detector is deterministic regardless of Telnyx chunk size.
	VADFrame = 20 * time.Millisecond
	// VADOnset is how long speech-level energy must persist before Start.
	VADOnset = 60 * time.Millisecond
	// VADHangover is how long silence must persist before End.
	VADHangover = 400 * time.Millisecond
	// VADThreshold is the RMS level, in int16 units, that counts as speech
	// (about -36 dBFS). Chosen as a conservative midpoint, unverified live.
	VADThreshold = 520.0
)

// SpeechTransition is a detector edge.
type SpeechTransition int

const (
	// SpeechNone means no edge at this sample.
	SpeechNone SpeechTransition = iota
	// SpeechStart is the caller beginning an utterance.
	SpeechStart
	// SpeechEnd is the caller finishing an utterance after the hangover.
	SpeechEnd
)

func (t SpeechTransition) String() string {
	switch t {
	case SpeechStart:
		return "start"
	case SpeechEnd:
		return "end"
	}
	return "none"
}

// VAD detects caller speech from linear 16-bit samples at a fixed rate. It is
// not safe for concurrent use; the owning media loop serialises calls.
type VAD struct {
	frameLen   int // samples per analysis frame
	onsetN     int // consecutive speech frames needed for Start
	hangoverN  int // consecutive silence frames needed for End
	acc        []int16
	speakingN  int // consecutive speech frames seen
	silentN    int // consecutive silence frames seen
	speaking   bool
	framesSeen uint64
}

// NewVAD creates a detector for sample rate rate.
func NewVAD(rate int) (*VAD, error) {
	f := Format{Encoding: L16, SampleRate: rate, LittleEndian: true}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	frameLen := f.Samples(VADFrame)
	if frameLen <= 0 {
		return nil, fmt.Errorf("audio: VAD frame too short at %d Hz", rate)
	}
	return &VAD{
		frameLen:  frameLen,
		onsetN:    ceilFrames(VADOnset),
		hangoverN: ceilFrames(VADHangover),
		acc:       make([]int16, 0, frameLen),
	}, nil
}

func ceilFrames(d time.Duration) int {
	return int((d + VADFrame - 1) / VADFrame)
}

// Speaking reports whether the detector is inside an utterance.
func (v *VAD) Speaking() bool { return v.speaking }

// Reset returns the detector to its silent start state, as if no utterance were
// open. It is used when the owner ends an utterance by policy (the forced turn
// bound) rather than by the detector's own hangover. A partial frame and the
// onset and hangover counters are cleared, so the next Start edge is judged
// from fresh audio.
func (v *VAD) Reset() {
	v.acc = v.acc[:0]
	v.speakingN = 0
	v.silentN = 0
	v.speaking = false
}

// Process consumes samples and returns the transitions they produce, in
// order. Partial frames carry over to the next call. A frame is judged only
// once complete, so the result does not depend on how samples are chunked.
func (v *VAD) Process(samples []int16) []SpeechTransition {
	var out []SpeechTransition
	for len(samples) > 0 {
		need := v.frameLen - len(v.acc)
		n := need
		if n > len(samples) {
			n = len(samples)
		}
		v.acc = append(v.acc, samples[:n]...)
		samples = samples[n:]
		if len(v.acc) < v.frameLen {
			break
		}
		if t := v.judge(rmsOf(v.acc)); t != SpeechNone {
			out = append(out, t)
		}
		v.acc = v.acc[:0]
	}
	return out
}

func (v *VAD) judge(rms float64) SpeechTransition {
	v.framesSeen++
	if rms >= VADThreshold {
		v.speakingN++
		v.silentN = 0
		if !v.speaking && v.speakingN >= v.onsetN {
			v.speaking = true
			return SpeechStart
		}
		return SpeechNone
	}
	v.silentN++
	v.speakingN = 0
	if v.speaking && v.silentN >= v.hangoverN {
		v.speaking = false
		return SpeechEnd
	}
	return SpeechNone
}

func rmsOf(s []int16) float64 {
	if len(s) == 0 {
		return 0
	}
	var sum float64
	for _, x := range s {
		f := float64(x)
		sum += f * f
	}
	return math.Sqrt(sum / float64(len(s)))
}
