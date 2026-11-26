// Package realtime defines the protocol-neutral voice session events and the
// per-protocol capability matrices. Protocol adapters live in subpackages
// realtimews (OpenAI Realtime WebSocket) and livews (GPT-Live primary
// WebSocket); they share no event schema.
package realtime

import (
	"errors"
	"fmt"

	"github.com/CallHarborMCP/callharbor/internal/audio"
)

// Protocol selects the AI voice protocol. It is never inferred from a model
// name or URL.
type Protocol string

const (
	ProtocolRealtime Protocol = "realtime_ws"
	ProtocolLive     Protocol = "live_ws"
)

// Availability states whether a capability exists for a protocol profile.
type Availability string

const (
	Native      Availability = "native"
	OptInBilled Availability = "opt_in_separately_billed"
	ClientSide  Availability = "client_side_only"
	Unavailable Availability = "unavailable"
)

// Capabilities is the documented capability matrix for one protocol, derived
// from the pinned upstream references in docs/protocol/FEASIBILITY.md.
type Capabilities struct {
	Protocol Protocol
	// Formats usable on the wire in both directions.
	Formats []audio.Format
	// ModelInURL is true when the model is a connect-URL query parameter.
	ModelInURL bool
	// CallerTranscript is user/inbound speech transcription.
	CallerTranscript Availability
	// AgentTranscript is model output transcript.
	AgentTranscript Availability
	// TranscriptTiming reports whether transcript fragments carry
	// session-relative timing.
	TranscriptTiming bool
	// ServerTruncate is true when the client can report the played
	// position so the server drops unheard output from context.
	ServerTruncate bool
	// ResponseCancel is true when the client can cancel model output.
	ResponseCancel bool
	// OutputPlayoutEvents are server events about audio playout; unavailable
	// on both WebSocket protocols.
	OutputPlayoutEvents Availability
	// Delegation is client-side delegation support.
	Delegation Availability
	// SpeechStartEvents is a provider event that reports the caller starting
	// to speak. Native means the engine can barge in on it; Unavailable means
	// the engine must use its local caller-speech detector instead.
	SpeechStartEvents Availability
	// DTMFInput is provider-side keypad input. It is unavailable on both
	// primary WebSocket protocols: DTMF arrives only on the telephony leg and
	// is never forwarded to the model, because digits may be PINs and no
	// documented input event carries them.
	DTMFInput Availability
}

var (
	pcmu = audio.Format{Encoding: audio.PCMU, SampleRate: 8000}
	pcma = audio.Format{Encoding: audio.PCMA, SampleRate: 8000}
)

// RealtimeCapabilities documents OpenAI Realtime GA over WebSocket.
var RealtimeCapabilities = Capabilities{
	Protocol: ProtocolRealtime,
	Formats: []audio.Format{
		{Encoding: audio.L16, SampleRate: 24000, LittleEndian: true}, pcmu, pcma,
	},
	ModelInURL:          true,
	CallerTranscript:    OptInBilled,
	AgentTranscript:     Native,
	TranscriptTiming:    false,
	ServerTruncate:      true,
	ResponseCancel:      true,
	OutputPlayoutEvents: Unavailable,
	Delegation:          Unavailable,
	SpeechStartEvents:   Native,
	DTMFInput:           Unavailable,
}

// LiveCapabilities documents GPT-Live over the primary WebSocket.
var LiveCapabilities = Capabilities{
	Protocol: ProtocolLive,
	Formats: []audio.Format{
		{Encoding: audio.L16, SampleRate: 24000, LittleEndian: true},
		{Encoding: audio.L16, SampleRate: 16000, LittleEndian: true},
		pcmu, pcma,
	},
	ModelInURL:          false,
	CallerTranscript:    Native,
	AgentTranscript:     Native,
	TranscriptTiming:    true,
	ServerTruncate:      false,
	ResponseCancel:      false,
	OutputPlayoutEvents: Unavailable,
	Delegation:          Native,
	SpeechStartEvents:   Unavailable,
	DTMFInput:           Unavailable,
}

// For returns the capability matrix for p.
func For(p Protocol) (Capabilities, error) {
	switch p {
	case ProtocolRealtime:
		return RealtimeCapabilities, nil
	case ProtocolLive:
		return LiveCapabilities, nil
	}
	return Capabilities{}, fmt.Errorf("realtime: unknown protocol %q", p)
}

// V1CallerTranscript is the caller transcript availability CallHarbor V1
// presents for c. Separately billed transcription is never enabled (owner
// decision recorded in docs/protocol/FEASIBILITY.md §4), so it is reported as
// Unavailable: caller audio is recorded, caller text is shown as unavailable.
func (c Capabilities) V1CallerTranscript() Availability {
	if c.CallerTranscript == Native {
		return Native
	}
	return Unavailable
}

// ErrCapability reports a required capability the selected profile lacks.
var ErrCapability = errors.New("realtime: required capability unavailable")

// Requirements are what a call needs from the voice profile.
type Requirements struct {
	Format audio.Format
	// TwoPartyTranscript requires both caller and agent transcripts without
	// any separately billed transcription service.
	TwoPartyTranscript bool
}

// Check fails closed when c cannot satisfy r.
func (c Capabilities) Check(r Requirements) error {
	ok := false
	for _, f := range c.Formats {
		if f == r.Format {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("%w: %s does not support audio format %v", ErrCapability, c.Protocol, r.Format)
	}
	if r.TwoPartyTranscript {
		switch c.CallerTranscript {
		case Native:
		case OptInBilled:
			return fmt.Errorf("%w: %s caller transcript requires a separately billed transcription model", ErrCapability, c.Protocol)
		default:
			return fmt.Errorf("%w: %s has no caller transcript", ErrCapability, c.Protocol)
		}
		if c.AgentTranscript == Unavailable {
			return fmt.Errorf("%w: %s has no agent transcript", ErrCapability, c.Protocol)
		}
	}
	return nil
}
