package telnyx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/audio"
	"github.com/CallHarborMCP/callharbor/internal/voicebridge"
)

// ErrFrameOrder reports a media frame that violates the start/stop ordering.
var ErrFrameOrder = errors.New("telnyx: media frame out of order")

// Send writes one CallHarbor→Telnyx text frame on the media stream.
type Send func(ctx context.Context, msg []byte) error

// MediaSink receives validated inbound frames for one media stream. Methods
// run on the connection read loop; a returned error closes the stream.
type MediaSink interface {
	Media(f Frame) error
	Mark(name string) error
	DTMF(digit string) error
	Error(f Frame) error
	Close() error
}

// SinkFactory creates the sink for a stream after its start frame validates.
// ctx is the stream's frame-loop context: it ends when the stream ends or the
// connection is cancelled, so setup work can stop with the connection.
type SinkFactory func(ctx context.Context, start Frame, send Send) (MediaSink, error)

// MediaEndpoint serves the Telnyx bidirectional media WebSocket. Every upgrade
// is authenticated with VerifyMediaHandshake against the exact streamURL that
// was given to Telnyx.
type MediaEndpoint struct {
	verifier  *Verifier
	streamURL string
	newSink   SinkFactory
}

// NewMediaEndpoint constructs the endpoint.
func NewMediaEndpoint(v *Verifier, streamURL string, newSink SinkFactory) (*MediaEndpoint, error) {
	if v == nil || streamURL == "" || newSink == nil {
		return nil, errors.New("telnyx: media endpoint requires verifier, stream URL and sink factory")
	}
	return &MediaEndpoint{verifier: v, streamURL: streamURL, newSink: newSink}, nil
}

// ServeHTTP authenticates the upgrade and runs the frame loop until Telnyx
// stops the stream, the client disconnects, or a frame is invalid.
func (e *MediaEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := e.verifier.VerifyMediaHandshake(r.Header, e.streamURL); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(MaxMediaFrame)
	ctx := r.Context()
	send := func(ctx context.Context, msg []byte) error {
		return c.Write(ctx, websocket.MessageText, msg)
	}
	var sink MediaSink
	defer func() {
		if sink != nil {
			_ = sink.Close()
		}
		_ = c.CloseNow()
	}()
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			_ = c.Close(websocket.StatusUnsupportedData, "binary frames are not part of the Telnyx media contract")
			return
		}
		f, err := ParseFrame(b)
		if err != nil {
			_ = c.Close(websocket.StatusPolicyViolation, "invalid media frame")
			return
		}
		if err := dispatch(ctx, &sink, f, send, e.newSink); err != nil {
			if errors.Is(err, errStop) {
				_ = c.Close(websocket.StatusNormalClosure, "stream stopped")
				return
			}
			_ = c.Close(websocket.StatusPolicyViolation, err.Error())
			return
		}
	}
}

var errStop = errors.New("telnyx: stream stopped")

func dispatch(ctx context.Context, sink *MediaSink, f Frame, send Send, newSink SinkFactory) error {
	switch f.Event {
	case FrameConnected:
		return nil
	case FrameStart:
		if *sink != nil {
			return fmt.Errorf("%w: duplicate start", ErrFrameOrder)
		}
		s, err := newSink(ctx, f, send)
		if err != nil {
			return err
		}
		*sink = s
		return nil
	case FrameStop:
		if *sink == nil {
			return fmt.Errorf("%w: stop before start", ErrFrameOrder)
		}
		return errStop
	}
	if *sink == nil {
		return fmt.Errorf("%w: %s before start", ErrFrameOrder, f.Event)
	}
	switch f.Event {
	case FrameMedia:
		return (*sink).Media(f)
	case FrameMark:
		return (*sink).Mark(f.MarkName)
	case FrameDTMF:
		return (*sink).DTMF(f.Digit)
	case FrameError:
		return (*sink).Error(f)
	}
	return fmt.Errorf("%w: unhandled event %q", ErrInvalidFrame, f.Event)
}

// BridgeSink connects one Telnyx media stream to a voicebridge.Bridge. Caller
// audio is converted and handed to ToProvider. Agent audio enters through
// AgentAudio, which sends the converted media and its playout mark back to
// Telnyx. Mark echoes update the playout ledger.
type BridgeSink struct {
	wmu     sync.Mutex
	mu      sync.Mutex
	bridge  *voicebridge.Bridge
	send    Send
	last    audio.SourceRef
	hasLast bool
	cut     map[string]bool
	// anonClosed gates agent audio that carries no item ID. It is closed by
	// a cut, and reopened by the caller's speech end edge (in Media) or by the
	// turn-length bound. Caller audio alone never reopens it, and no
	// unidentified chunk can bypass a closed gate.
	anonClosed bool
	// turn counts caller turns; inTurn is true from a Start edge to its End.
	// InterruptTurn cuts only while the caller is still in the turn it names.
	// turnStart is when the open turn began; a turn that stays open past
	// MaxCallerTurn is force-ended so a stuck detector cannot hold the gate.
	turn       uint64
	inTurn     bool
	turnStart  time.Time
	now        func() time.Time
	ToProvider func(provider []byte) error
	OnDTMF     func(digit string)
	OnError    func(f Frame)
	// OnCallerSpeech receives caller speech edges from the local detector.
	// It runs on the inbound read loop and must not block.
	OnCallerSpeech func(ev SpeechEvent)
}

// MaxPendingMarks caps playout marks awaiting an echo. Telnyx echoes every
// mark, so a stalled leg grows this count; past the cap the stream refuses
// more agent audio with ErrPlayoutBackpressure rather than growing memory.
// Expired marks do not count toward the cap.
const MaxPendingMarks = 4096

// ErrPlayoutBackpressure is returned by AgentAudio when MaxPendingMarks
// unexpired marks are awaiting echoes. It is an outbound playout limit, not an
// inbound message failure, so it is distinct from ErrStreamBackpressure.
var ErrPlayoutBackpressure = errors.New("telnyx: playout marks awaiting echo exceed cap")

// MaxCallerTurn bounds one caller turn. A turn the local detector never ends
// (stuck noise, a media pause) is force-ended after this long, reopening the
// anonymous gate. It is a live-unverified constant: a genuine turn longer than
// this is also force-ended, so a long monologue reopens the gate early.
const MaxCallerTurn = 15 * time.Second

// SpeechEvent is one caller speech edge with its wall-clock time.
type SpeechEvent struct {
	Started bool
	At      time.Time
	// Turn numbers the caller turn this edge belongs to. Every Start edge
	// begins a new turn; the matching End edge carries the same number.
	Turn uint64
	// Audible is set on a Start edge when agent audio was still playing at the
	// moment the edge was observed. It is decided with the edge, under the sink
	// lock, so the engine never has to judge a stale edge against later state.
	Audible bool
}

// Cut describes what an interruption cut. ItemID is empty when the cut audio
// carried no provider item ID. Played is the estimated audible duration of the
// cut item and Sent is the duration handed to Telnyx; Audible reports whether
// any agent audio was still playing when the cut happened.
type Cut struct {
	ItemID       string
	ContentIndex int
	Played       time.Duration
	Sent         time.Duration
	Audible      bool
}

// NewBridgeSink binds a bridge to a Telnyx stream.
func NewBridgeSink(b *voicebridge.Bridge, send Send) (*BridgeSink, error) {
	if b == nil || send == nil {
		return nil, errors.New("telnyx: bridge sink requires a bridge and sender")
	}
	return &BridgeSink{bridge: b, send: send, cut: make(map[string]bool), now: time.Now}, nil
}

// SetClock replaces the wall clock. Tests use it to make timing deterministic.
// It must be called before the stream starts.
func (s *BridgeSink) SetClock(now func() time.Time) { s.now = now }

// Media converts one caller payload, forwards it to the provider, and reports
// any caller speech edges the local detector produced.
func (s *BridgeSink) Media(f Frame) error {
	if f.Track != "inbound" {
		return nil
	}
	s.mu.Lock()
	p, err := s.bridge.CallerAudio(f.Payload)
	edges := s.bridge.TakeSpeech()
	now := s.now()
	// Each edge is stamped with its turn and, for a Start, with whether agent
	// audio is audible at this instant. Both are decided under s.mu, so a later
	// InterruptTurn sees exactly the state this edge saw.
	stamped := make([]SpeechEvent, 0, len(edges))
	if ev, ok := s.expireTurnLocked(now); ok {
		stamped = append(stamped, ev)
	}
	for _, e := range edges {
		switch e {
		case audio.SpeechStart:
			s.turn++
			s.inTurn = true
			s.turnStart = now
			stamped = append(stamped, SpeechEvent{Started: true, At: now, Turn: s.turn, Audible: s.bridge.Playing(now)})
		case audio.SpeechEnd:
			s.inTurn = false
			// An end edge reopens the anonymous turn here, synchronously, before
			// the caller frame that ended the turn returns. A reply that the
			// provider sends after the caller stops is then never gated by a
			// closed turn that the speech pump has not reached yet.
			s.anonClosed = false
			stamped = append(stamped, SpeechEvent{Started: false, At: now, Turn: s.turn})
		}
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if s.OnCallerSpeech != nil {
		for _, ev := range stamped {
			s.OnCallerSpeech(ev)
		}
	}
	if s.ToProvider != nil {
		return s.ToProvider(p)
	}
	return nil
}

// expireTurnLocked force-ends a caller turn that has stayed open past
// MaxCallerTurn. A turn the detector never ends (stuck line noise, or a media
// pause that leaves the hangover unreached) would otherwise hold the anonymous
// gate closed until max_duration, and the silence watchdog would never fire.
// The caller holds mu. It returns the synthetic End edge it produced, if any.
func (s *BridgeSink) expireTurnLocked(now time.Time) (SpeechEvent, bool) {
	if !s.inTurn || now.Sub(s.turnStart) < MaxCallerTurn {
		return SpeechEvent{}, false
	}
	s.inTurn = false
	s.anonClosed = false
	return SpeechEvent{Started: false, At: now, Turn: s.turn}, true
}

// Mark records a playout echo from Telnyx.
func (s *BridgeSink) Mark(name string) error {
	s.mu.Lock()
	now := s.now()
	_ = s.bridge.Expire(now)
	_, err := s.bridge.Echo(name, now)
	s.mu.Unlock()
	return err
}

// DTMF forwards a keypress to the call layer.
func (s *BridgeSink) DTMF(digit string) error {
	if s.OnDTMF != nil {
		s.OnDTMF(digit)
	}
	return nil
}

// Error forwards a Telnyx stream error to the call layer.
func (s *BridgeSink) Error(f Frame) error {
	if s.OnError != nil {
		s.OnError(f)
	}
	return nil
}

// Close releases the stream. The bridge owns no network resources.
func (s *BridgeSink) Close() error { return nil }

// AgentAudio sends one provider output chunk to Telnyx followed by its mark.
// wmu is held from the cut check through the media and mark writes, so a
// concurrent Interrupt cannot put clear between them or before them.
//
// Gating: an item ID that was cut suppresses that item. Audio without an item
// ID is suppressed while the anonymous turn is closed. A cut closes the gate,
// and the caller's speech end edge (or the turn-length bound) reopens it, both
// inside Media. Because the check is by the presence of an item ID alone, no
// unidentified chunk can bypass a closed gate.
//
// A full pending-mark ledger returns ErrPlayoutBackpressure before anything is
// sent, so the stream never writes media it cannot account for.
func (s *BridgeSink) AgentAudio(ctx context.Context, src audio.SourceRef, provider []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	gated := s.anonClosed
	if src.ItemID != "" {
		gated = s.cut[src.ItemID]
	}
	if gated {
		s.mu.Unlock()
		return nil
	}
	now := s.now()
	// Overdue marks are only counted here; the expiry error is not a stream
	// failure, and the unconfirmed audio is already accounted for in the ledger.
	_ = s.bridge.Expire(now)
	if s.bridge.PendingLive() >= MaxPendingMarks {
		s.mu.Unlock()
		return ErrPlayoutBackpressure
	}
	s.last, s.hasLast = src, true
	payload, mark, err := s.bridge.AgentAudio(src, provider, now)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	media, err := MediaMessage(payload)
	if err != nil {
		return err
	}
	if err := s.send(ctx, media); err != nil {
		return err
	}
	markMsg, err := MarkMessage(mark)
	if err != nil {
		return err
	}
	return s.send(ctx, markMsg)
}

// Interrupt cuts agent audio unconditionally: queued audio is dropped, the
// anonymous gate closes, and Telnyx is sent a clear. The cut and clear are
// ordered after any agent media already being written. It returns what was cut
// so the engine can truncate the provider's record of the item to the audio the
// caller actually heard.
func (s *BridgeSink) Interrupt(ctx context.Context) (Cut, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	cut := s.cutLocked()
	s.mu.Unlock()
	return cut, s.send(ctx, ClearMessage())
}

// InterruptTurn cuts agent audio only while the caller is still in turn, the
// turn number of the Start edge that requested the cut. If the caller has since
// ended that turn, or a later turn has begun, the cut is refused: no clear is
// sent and ok is false. The check and the cut happen under the same locks as
// AgentAudio and Media, so they cannot interleave with a reply or an edge.
func (s *BridgeSink) InterruptTurn(ctx context.Context, turn uint64) (Cut, bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	if s.turn != turn || !s.inTurn {
		s.mu.Unlock()
		return Cut{}, false, nil
	}
	cut := s.cutLocked()
	s.mu.Unlock()
	return cut, true, s.send(ctx, ClearMessage())
}

// cutLocked records and performs the cut. The caller holds wmu and mu.
func (s *BridgeSink) cutLocked() Cut {
	var cut Cut
	if s.hasLast {
		cut.ItemID, cut.ContentIndex = s.last.ItemID, s.last.ContentIndex
		if s.last.ItemID != "" {
			s.cut[s.last.ItemID] = true
		}
	}
	s.anonClosed = true
	cut.Audible = s.bridge.Interrupt(s.now())
	// Read after the clear: Clear adds the in-flight played portion to the
	// item's estimate, which is the value truncate must report.
	if cut.ItemID != "" {
		if it, ok := s.bridge.Item(s.last); ok {
			cut.Played, cut.Sent = s.bridge.Position(s.last, s.now()), it.Sent
		}
	}
	return cut
}

// PlayoutEnd is when queued agent audio finishes at real-time rate. The engine
// reads it to decide whether agent audio is audible at a caller speech onset.
func (s *BridgeSink) PlayoutEnd() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bridge.PlayoutEnd()
}

// ReopenTurn opens the anonymous turn after the caller has finished speaking,
// so the model's next reply, which carries no item ID, can play. Identified
// items cut earlier stay suppressed. Media already calls this on every speech
// end edge; it is exported for transports that learn of the end elsewhere.
func (s *BridgeSink) ReopenTurn() {
	s.mu.Lock()
	s.anonClosed = false
	s.mu.Unlock()
}
