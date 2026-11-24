package voicecall

import (
	"context"
	"errors"
	"sync"

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
	done   chan struct{}
	once   sync.Once

	mu       sync.Mutex
	lastItem audio.SourceRef
	hasItem  bool
}

var _ voice.AudioBridge = (*AudioBridge)(nil)

// NewAudioBridge wraps sink, which must already be bound to a Telnyx stream.
// Caller audio that the sink converts is routed into the engine queue through
// ToProvider, so the sink must be created before the bridge and not reassigned.
func NewAudioBridge(sink *telnyx.BridgeSink) (*AudioBridge, error) {
	if sink == nil {
		return nil, errors.New("voicecall: audio bridge requires a bridge sink")
	}
	b := &AudioBridge{sink: sink, caller: make(chan []byte, callerQueue), done: make(chan struct{})}
	sink.ToProvider = b.enqueueCaller
	return b, nil
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

// Interrupt drops unplayed model audio and sends Telnyx the clear command.
func (b *AudioBridge) Interrupt(ctx context.Context) error {
	return b.sink.Interrupt(ctx)
}

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
