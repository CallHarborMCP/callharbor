// Package voicebridge is the protocol-neutral audio bridge between a telephony
// media leg (Telnyx media WebSocket, G.711 or L16 frames) and an AI voice
// provider (Realtime or GPT-Live WebSocket). It converts codecs and sample
// rates in both directions and records outbound playout in a PlayoutLedger so
// the played-audio truth survives barge-in. It knows nothing about Telnyx or
// provider event schemas; adapters translate those into calls on Bridge.
package voicebridge

import (
	"errors"
	"fmt"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
)

var ErrFormat = errors.New("voicebridge: unsupported audio format")

// Bridge converts audio between the telephony leg and the provider leg.
type Bridge struct {
	leg      audio.Format
	provider audio.Format
	ledger   *audio.PlayoutLedger
	inRate   *audio.Resampler
	outRate  *audio.Resampler
}

// New creates a bridge for one call. leg is the telephony wire format (G.711
// 8 kHz on Telnyx). provider is the format negotiated with the AI voice
// protocol. tolerance and grace configure the playout ledger.
func New(leg, provider audio.Format, tolerance, grace time.Duration) (*Bridge, error) {
	if err := leg.Validate(); err != nil {
		return nil, fmt.Errorf("%w: leg: %v", ErrFormat, err)
	}
	if err := provider.Validate(); err != nil {
		return nil, fmt.Errorf("%w: provider: %v", ErrFormat, err)
	}
	l, err := audio.NewPlayoutLedger(leg, tolerance, grace)
	if err != nil {
		return nil, err
	}
	b := &Bridge{leg: leg, provider: provider, ledger: l}
	if leg.SampleRate != provider.SampleRate {
		if b.inRate, err = audio.NewResampler(leg.SampleRate, provider.SampleRate); err != nil {
			return nil, err
		}
		if b.outRate, err = audio.NewResampler(provider.SampleRate, leg.SampleRate); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// CallerAudio converts one inbound telephony payload to provider wire bytes.
func (b *Bridge) CallerAudio(payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return nil, errors.New("voicebridge: empty caller payload")
	}
	s, err := audio.Decode(nil, b.leg, payload)
	if err != nil {
		return nil, err
	}
	if b.inRate != nil {
		s = b.inRate.Process(nil, s)
	}
	return audio.Encode(nil, b.provider, s)
}

// AgentAudio converts one provider output chunk to telephony wire bytes and
// records it in the playout ledger. The caller must transmit the returned
// media payload followed by the returned mark name.
func (b *Bridge) AgentAudio(src audio.SourceRef, providerBytes []byte, now time.Time) (payload []byte, mark string, err error) {
	if len(providerBytes) == 0 {
		return nil, "", errors.New("voicebridge: empty agent audio")
	}
	s, err := audio.Decode(nil, b.provider, providerBytes)
	if err != nil {
		return nil, "", err
	}
	if b.outRate != nil {
		s = b.outRate.Process(nil, s)
	}
	out, err := audio.Encode(nil, b.leg, s)
	if err != nil {
		return nil, "", err
	}
	if len(out) == 0 {
		return nil, "", errors.New("voicebridge: agent chunk too short to transmit")
	}
	mark, err = b.ledger.Send(src, len(out), now)
	if err != nil {
		return nil, "", err
	}
	return out, mark, nil
}

// Echo classifies a mark echoed by the telephony leg.
func (b *Bridge) Echo(name string, now time.Time) (audio.MarkOutcome, error) {
	return b.ledger.Echo(name, now)
}

// Interrupt records a barge-in cut in the ledger. The caller must also send
// the telephony clear command. Provider barge-in signals are not playback
// confirmation; only echoed marks are.
func (b *Bridge) Interrupt(now time.Time) {
	b.ledger.Clear(now)
}

// Expire reports playout marks that never echoed within the ledger grace.
func (b *Bridge) Expire(now time.Time) error {
	return b.ledger.Expire(now)
}

// Item returns the playout accounting for one provider item.
func (b *Bridge) Item(src audio.SourceRef) (audio.ItemPlayout, bool) {
	return b.ledger.Item(src)
}

// Pending reports marks still awaiting an echo.
func (b *Bridge) Pending() int { return b.ledger.Pending() }
