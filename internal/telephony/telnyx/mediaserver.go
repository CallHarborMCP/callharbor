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
type SinkFactory func(start Frame, send Send) (MediaSink, error)

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
		s, err := newSink(f, send)
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
	mu         sync.Mutex
	bridge     *voicebridge.Bridge
	send       Send
	lastItem   string
	hasLast    bool
	cut        map[string]bool
	ToProvider func(provider []byte) error
	OnDTMF     func(digit string)
	OnError    func(f Frame)
}

// NewBridgeSink binds a bridge to a Telnyx stream.
func NewBridgeSink(b *voicebridge.Bridge, send Send) (*BridgeSink, error) {
	if b == nil || send == nil {
		return nil, errors.New("telnyx: bridge sink requires a bridge and sender")
	}
	return &BridgeSink{bridge: b, send: send, cut: make(map[string]bool)}, nil
}

// Media converts one caller payload and forwards it to the provider.
func (s *BridgeSink) Media(f Frame) error {
	if f.Track != "inbound" {
		return nil
	}
	s.mu.Lock()
	p, err := s.bridge.CallerAudio(f.Payload)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if s.ToProvider != nil {
		return s.ToProvider(p)
	}
	return nil
}

// Mark records a playout echo from Telnyx.
func (s *BridgeSink) Mark(name string) error {
	s.mu.Lock()
	_, err := s.bridge.Echo(name, time.Now())
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
func (s *BridgeSink) AgentAudio(ctx context.Context, src audio.SourceRef, provider []byte) error {
	s.mu.Lock()
	if src.ItemID != "" && s.cut[src.ItemID] {
		s.mu.Unlock()
		return nil
	}
	s.lastItem, s.hasLast = src.ItemID, true
	payload, mark, err := s.bridge.AgentAudio(src, provider, time.Now())
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

// Interrupt records a barge-in cut and sends Telnyx the clear command.
func (s *BridgeSink) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	s.bridge.Interrupt(time.Now())
	if s.hasLast && s.lastItem != "" {
		s.cut[s.lastItem] = true
	}
	s.mu.Unlock()
	return s.send(ctx, ClearMessage())
}
