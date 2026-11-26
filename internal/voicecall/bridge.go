package voicecall

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/realtime"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
	"github.com/CallHarborMCP/callharbor/internal/voice"
)

// ErrBridgeClosed reports that the caller's media leg ended.
var ErrBridgeClosed = errors.New("voicecall: media bridge closed")

// callerQueue bounds caller audio waiting for the voice engine. A full queue
// is an overload: the stream reports backpressure rather than growing memory.
const callerQueue = 64

// AudioBridge adapts a Telnyx media sink to voice.AudioBridge. Caller audio
// arrives push-style from Telnyx and is pulled by the engine through a bounded
// queue. Model audio is pushed out through the sink with its item identity so
// barge-in can gate it per item.
type AudioBridge struct {
	sink *telnyx.BridgeSink

	caller chan []byte
	// speech carries caller speech edges to the engine. Its bound is
	// speechQueue; a full channel drops the edge (see enqueueSpeech).
	speech chan voice.SpeechEvent
	done   chan struct{}
	once   sync.Once

	mu       sync.Mutex
	lastItem audio.SourceRef
	hasItem  bool
}

var (
	_ voice.AudioBridge  = (*AudioBridge)(nil)
	_ voice.SpeechBridge = (*AudioBridge)(nil)
)

// speechQueue bounds unread caller speech edges. The engine drains them
// promptly; a lagging reader loses the oldest edge, never blocks the media loop.
const speechQueue = 8

// NewAudioBridge wraps sink, which must already be bound to a Telnyx stream.
// Caller audio that the sink converts is routed into the engine queue through
// ToProvider, so the sink must be created before the bridge and not reassigned.
func NewAudioBridge(sink *telnyx.BridgeSink) (*AudioBridge, error) {
	if sink == nil {
		return nil, errors.New("voicecall: audio bridge requires a bridge sink")
	}
	b := &AudioBridge{
		sink:   sink,
		caller: make(chan []byte, callerQueue),
		speech: make(chan voice.SpeechEvent, speechQueue),
		done:   make(chan struct{}),
	}
	sink.ToProvider = b.enqueueCaller
	sink.OnCallerSpeech = b.enqueueSpeech
	return b, nil
}

// enqueueSpeech is non-blocking. When the engine lags, the oldest edge is
// dropped so the newest state is always delivered. A start edge may be lost:
// it only drives the onset cut, and the next end edge still resets silence
// accounting. An end edge is never dropped: the engine's turn state depends on
// it, so a lost end would leave the caller's turn open.
func (b *AudioBridge) enqueueSpeech(ev telnyx.SpeechEvent) {
	out := voice.SpeechEvent{Started: ev.Started, At: ev.At, Turn: ev.Turn, Audible: ev.Audible}
	for {
		select {
		case b.speech <- out:
			return
		default:
		}
		if !ev.Started {
			// Make room for the end edge by dropping the oldest queued edge.
			select {
			case <-b.speech:
			default:
			}
			continue
		}
		// A start edge that finds the queue full is dropped, not waited on.
		return
	}
}

func (b *AudioBridge) enqueueCaller(chunk []byte) error {
	select {
	case <-b.done:
		return ErrBridgeClosed
	default:
	}
	select {
	case b.caller <- append([]byte(nil), chunk...):
		return nil
	default:
		return telnyx.ErrStreamBackpressure
	}
}

// Close ends the caller leg. Pending NextCallerAudio calls return
// ErrBridgeClosed. It is safe to call more than once.
func (b *AudioBridge) Close() {
	b.once.Do(func() { close(b.done) })
}

// NextCallerAudio blocks until a caller chunk is available or ctx ends.
func (b *AudioBridge) NextCallerAudio(ctx context.Context) ([]byte, error) {
	select {
	case chunk := <-b.caller:
		return chunk, nil
	case <-b.done:
		return nil, ErrBridgeClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Play sends one model audio chunk to Telnyx, tagged with the provider item it
// belongs to, so barge-in gating applies per item.
func (b *AudioBridge) Play(ctx context.Context, pcm []byte) error {
	return b.sink.AgentAudio(ctx, b.currentItem(), pcm)
}

// Interrupt drops unplayed model audio, sends Telnyx the clear command and
// returns what was cut, in provider-neutral terms.
func (b *AudioBridge) Interrupt(ctx context.Context) (voice.Cut, error) {
	c, err := b.sink.Interrupt(ctx)
	if err != nil {
		return voice.Cut{}, err
	}
	return voice.Cut{ItemID: c.ItemID, ContentIndex: c.ContentIndex, Played: c.Played, Sent: c.Sent, Audible: c.Audible}, nil
}

// CallerSpeech returns the bounded channel of caller speech edges.
// InterruptTurn cuts agent audio only while the caller is still in turn. A
// refused cut returns ok false and sends nothing to Telnyx.
func (b *AudioBridge) InterruptTurn(ctx context.Context, turn uint64) (voice.Cut, bool, error) {
	c, ok, err := b.sink.InterruptTurn(ctx, turn)
	if err != nil || !ok {
		return voice.Cut{}, ok, err
	}
	return voice.Cut{ItemID: c.ItemID, ContentIndex: c.ContentIndex, Played: c.Played, Sent: c.Sent, Audible: c.Audible}, true, nil
}

func (b *AudioBridge) CallerSpeech() <-chan voice.SpeechEvent { return b.speech }

// ReopenTurn reopens the anonymous agent-audio turn after the caller stops.
func (b *AudioBridge) ReopenTurn() { b.sink.ReopenTurn() }

// PlayoutEnd reports when queued agent audio finishes. Without it the engine
// cannot tell that agent audio is audible, so local barge-in on live_ws never
// cuts.
func (b *AudioBridge) PlayoutEnd() time.Time { return b.sink.PlayoutEnd() }

// Item records the provider item the next Play belongs to. The engine calls it
// from the event sink, before Play, on the same goroutine.
func (b *AudioBridge) Item(src audio.SourceRef) {
	b.mu.Lock()
	b.lastItem, b.hasItem = src, true
	b.mu.Unlock()
}

func (b *AudioBridge) currentItem() audio.SourceRef {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastItem
}

// EventSink returns a voice.EventSink that tracks the provider item of each
// audio event and forwards every event to next when it is non-nil.
func (b *AudioBridge) EventSink(next voice.EventSink) voice.EventSink {
	return func(ev realtime.Event) {
		if ev.Kind == realtime.EventAudio {
			b.Item(audio.SourceRef{ItemID: ev.ItemID, ContentIndex: ev.ContentIndex})
		}
		if next != nil {
			next(ev)
		}
	}
}
