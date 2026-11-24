package telnyx

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/CallHarborMCP/callharbor/internal/audio"
)

var (
	ErrUnauthorizedStream = errors.New("telnyx: media stream not authorized")
	ErrStreamProtocol     = errors.New("telnyx: media stream protocol violation")
	ErrStreamBackpressure = errors.New("telnyx: inbound media consumer too slow")
	ErrStreamQueueFull    = errors.New("telnyx: outbound media queue full")
	ErrStreamClosed       = errors.New("telnyx: media session closed")
	ErrStreamIdle         = errors.New("telnyx: media stream idle timeout")
)

type streamGrant struct {
	callControlID string
	expires       time.Time
}

// StreamAuth issues and redeems single-use media WebSocket tokens bound to one
// call. Only SHA-256 digests of tokens are retained.
type StreamAuth struct {
	mu     sync.Mutex
	grants map[[sha256.Size]byte]streamGrant
}

func NewStreamAuth() *StreamAuth {
	return &StreamAuth{grants: map[[sha256.Size]byte]streamGrant{}}
}

// Issue returns a fresh 256-bit token valid for ttl that authorises exactly one
// media stream for callControlID.
func (a *StreamAuth) Issue(callControlID string, ttl time.Duration, now time.Time) (string, error) {
	if callControlID == "" || ttl <= 0 {
		return "", fmt.Errorf("%w: issue requires a call and positive ttl", ErrUnauthorizedStream)
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw[:])
	a.mu.Lock()
	defer a.mu.Unlock()
	for d, g := range a.grants {
		if now.After(g.expires) {
			delete(a.grants, d)
		}
	}
	a.grants[sha256.Sum256([]byte(tok))] = streamGrant{callControlID: callControlID, expires: now.Add(ttl)}
	return tok, nil
}

// Redeem consumes token and returns the call it authorises. Unknown, reused
// and expired tokens are rejected identically.
func (a *StreamAuth) Redeem(token string, now time.Time) (string, error) {
	if token == "" {
		return "", ErrUnauthorizedStream
	}
	d := sha256.Sum256([]byte(token))
	a.mu.Lock()
	defer a.mu.Unlock()
	g, ok := a.grants[d]
	if !ok {
		return "", ErrUnauthorizedStream
	}
	delete(a.grants, d)
	if now.After(g.expires) {
		return "", ErrUnauthorizedStream
	}
	return g.callControlID, nil
}

// StreamConfig bounds one media session. Every queue is finite: overflow fails
// the session or returns an explicit error rather than growing memory.
type StreamConfig struct {
	Format           audio.Format
	InboundQueue     int
	JitterFrames     int
	OutboundQueue    int
	MaxChunkBytes    int
	IdleTimeout      time.Duration
	ExpireEvery      time.Duration
	PlayoutTolerance time.Duration
	PlayoutGrace     time.Duration
	Now              func() time.Time
}

func (c StreamConfig) validate() error {
	if err := c.Format.Validate(); err != nil {
		return err
	}
	if c.InboundQueue <= 0 || c.JitterFrames < 0 || c.OutboundQueue <= 0 || c.MaxChunkBytes <= 0 {
		return fmt.Errorf("%w: queue bounds must be positive", ErrInvalidFrame)
	}
	if c.IdleTimeout <= 0 || c.ExpireEvery <= 0 || c.PlayoutGrace <= 0 || c.PlayoutTolerance < 0 {
		return fmt.Errorf("%w: timing bounds must be positive", ErrInvalidFrame)
	}
	if c.Now == nil {
		return fmt.Errorf("%w: clock required", ErrInvalidFrame)
	}
	return nil
}

// InboundAudio is one caller-track chunk in media timeline order. Position and
// Gap are sample offsets on the stream's RTP clock.
type InboundAudio struct {
	Position int64
	Gap      int64
	Payload  []byte
}

type outItem struct {
	gen     uint64
	src     audio.SourceRef
	payload []byte
}

// StreamStats are counters for one session.
type StreamStats struct {
	InboundChunks     int
	Duplicates        int
	Stale             int
	GapSamples        int64
	OtherTrack        int
	DTMF              int
	OutboundChunks    int
	OutboundDiscarded int
	PlayoutTimeouts   int
	MarksPlayed       int
	MarksFlushed      int
	MarksEarly        int
}

// Result is final accounting for a session, valid once Done is closed.
type Result struct {
	Stats StreamStats
	Items []audio.ItemPlayout
}

// MediaSession is one authenticated Telnyx bidirectional media stream bound to
// one call. Inbound audio and DTMF are delivered on bounded channels; outbound
// audio is queued with Enqueue and interrupted with Clear.
type MediaSession struct {
	callControlID string
	cfg           StreamConfig
	bps           int

	audio chan InboundAudio
	dtmf  chan string
	out   chan outItem
	clear chan struct{}
	done  chan struct{}

	mu  sync.Mutex
	gen uint64

	streamID  string
	started   bool
	connected bool
	clock     audio.RTPClock
	next      int64
	jit       []Frame
	ledger    *audio.PlayoutLedger
	stats     StreamStats
	result    Result
	err       error
}

func newMediaSession(callControlID string, cfg StreamConfig) (*MediaSession, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	l, err := audio.NewPlayoutLedger(cfg.Format, cfg.PlayoutTolerance, cfg.PlayoutGrace)
	if err != nil {
		return nil, err
	}
	return &MediaSession{
		callControlID: callControlID,
		cfg:           cfg,
		bps:           cfg.Format.BytesPerSample(),
		audio:         make(chan InboundAudio, cfg.InboundQueue),
		dtmf:          make(chan string, 16),
		out:           make(chan outItem, cfg.OutboundQueue),
		clear:         make(chan struct{}, 1),
		done:          make(chan struct{}),
		ledger:        l,
	}, nil
}

func (s *MediaSession) CallControlID() string { return s.callControlID }

// Audio delivers inbound caller audio in timeline order.
func (s *MediaSession) Audio() <-chan InboundAudio { return s.audio }

// DTMF delivers caller keypad digits.
func (s *MediaSession) DTMF() <-chan string { return s.dtmf }

// Done is closed when the session has ended; Result and Err are then valid.
func (s *MediaSession) Done() <-chan struct{} { return s.done }

// Err returns the reason the session ended, nil for a clean stop.
func (s *MediaSession) Err() error {
	<-s.done
	return s.err
}

// Result returns final accounting once the session has ended.
func (s *MediaSession) Result() Result {
	<-s.done
	return s.result
}

// Enqueue queues one encoded chunk of agent audio for playout. It never
// blocks: a full queue or a finished session returns an explicit error.
func (s *MediaSession) Enqueue(src audio.SourceRef, payload []byte) error {
	select {
	case <-s.done:
		return ErrStreamClosed
	default:
	}
	if len(payload) == 0 || len(payload) > s.cfg.MaxChunkBytes {
		return fmt.Errorf("telnyx: chunk of %d bytes outside (0,%d]", len(payload), s.cfg.MaxChunkBytes)
	}
	if len(payload)%s.bps != 0 {
		return fmt.Errorf("telnyx: chunk of %d bytes is not whole %s samples", len(payload), s.cfg.Format)
	}
	s.mu.Lock()
	gen := s.gen
	s.mu.Unlock()
	select {
	case s.out <- outItem{gen: gen, src: src, payload: payload}:
		return nil
	default:
		return ErrStreamQueueFull
	}
}

// Clear interrupts playout (barge-in). Chunks enqueued before Clear are
// discarded; the Telnyx clear command is sent before any later chunk.
func (s *MediaSession) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	select {
	case s.clear <- struct{}{}:
	default:
	}
}

// MediaHandler serves Telnyx bidirectional media WebSockets. Authentication is
// the Telnyx Ed25519 handshake signature over the exact stream URL plus a
// single-use, per-call path token. stream_auth_token delivery is undocumented
// and is deliberately not relied upon.
type MediaHandler struct {
	Verifier *Verifier
	Auth     *StreamAuth
	// BaseURL is the public wss:// prefix ending in "/", the same prefix used
	// to build each stream_url. The token path segment is appended to it.
	BaseURL string
	Config  StreamConfig
	// Accept is called once per authenticated session, before any media frame.
	Accept func(*MediaSession)
}

func (h *MediaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	streamURL := h.BaseURL + token
	if r.URL.RawQuery != "" {
		streamURL += "?" + r.URL.RawQuery
	}
	if err := h.Verifier.VerifyMediaHandshake(r.Header, streamURL); err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	callID, err := h.Auth.Redeem(token, h.Config.Now())
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s, err := newMediaSession(callID, h.Config)
	if err != nil {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(MaxMediaFrame)
	if h.Accept != nil {
		h.Accept(s)
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	err = s.serve(ctx, conn)
	s.err = err
	s.result = Result{Stats: s.stats, Items: s.ledger.Items()}
	close(s.done)
	status, reason := websocket.StatusNormalClosure, "stopped"
	if err != nil {
		status, reason = websocket.StatusPolicyViolation, "failed"
	}
	_ = conn.Close(status, reason)
}

func (s *MediaSession) serve(ctx context.Context, conn *websocket.Conn) error {
	frames := make(chan Frame, 16)
	readErr := make(chan error, 1)
	go func() {
		for {
			typ, b, err := conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			if typ != websocket.MessageText {
				readErr <- fmt.Errorf("%w: binary frame", ErrStreamProtocol)
				return
			}
			f, err := ParseFrame(b)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()

	idle := time.NewTimer(s.cfg.IdleTimeout)
	defer idle.Stop()
	expire := time.NewTicker(s.cfg.ExpireEvery)
	defer expire.Stop()
	for {
		select {
		case err := <-readErr:
			return fmt.Errorf("%w: socket ended before stop: %v", ErrStreamProtocol, err)
		case <-ctx.Done():
			return ctx.Err()
		case <-idle.C:
			return ErrStreamIdle
		case <-expire.C:
			if err := s.ledger.Expire(s.cfg.Now()); err != nil {
				s.stats.PlayoutTimeouts++
			}
		case <-s.clear:
			if err := s.doClear(ctx, conn); err != nil {
				return err
			}
		case it := <-s.out:
			if err := s.sendChunk(ctx, conn, it); err != nil {
				return err
			}
		case f := <-frames:
			idle.Reset(s.cfg.IdleTimeout)
			stop, err := s.handle(f)
			if err != nil {
				return err
			}
			if stop {
				return s.flush()
			}
		}
	}
}

func (s *MediaSession) handle(f Frame) (bool, error) {
	switch f.Event {
	case FrameConnected:
		if s.connected || s.started {
			return false, fmt.Errorf("%w: connected out of order", ErrStreamProtocol)
		}
		s.connected = true
	case FrameStart:
		if s.started {
			return false, fmt.Errorf("%w: duplicate start", ErrStreamProtocol)
		}
		if f.CallControlID != s.callControlID {
			return false, fmt.Errorf("%w: start for a different call", ErrUnauthorizedStream)
		}
		if f.Format != s.cfg.Format {
			return false, fmt.Errorf("%w: stream format %v, negotiated %v", ErrStreamProtocol, f.Format, s.cfg.Format)
		}
		s.streamID, s.started = f.StreamID, true
	case FrameMedia:
		if !s.started || f.StreamID != s.streamID {
			return false, fmt.Errorf("%w: media before start or for another stream", ErrStreamProtocol)
		}
		if f.Track != "inbound" {
			s.stats.OtherTrack++
			return false, nil
		}
		s.push(f)
		for len(s.jit) > s.cfg.JitterFrames {
			if err := s.play(s.pop()); err != nil {
				return false, err
			}
		}
	case FrameMark:
		if !s.started {
			return false, fmt.Errorf("%w: mark before start", ErrStreamProtocol)
		}
		outcome, err := s.ledger.Echo(f.MarkName, s.cfg.Now())
		if err != nil {
			return false, fmt.Errorf("%w: %v", ErrStreamProtocol, err)
		}
		switch outcome {
		case audio.MarkPlayed:
			s.stats.MarksPlayed++
		case audio.MarkFlushed:
			s.stats.MarksFlushed++
		default:
			s.stats.MarksEarly++
		}
	case FrameDTMF:
		if !s.started {
			return false, fmt.Errorf("%w: dtmf before start", ErrStreamProtocol)
		}
		select {
		case s.dtmf <- f.Digit:
			s.stats.DTMF++
		default:
			return false, ErrStreamBackpressure
		}
	case FrameError:
		return false, fmt.Errorf("%w: telnyx error %d: %s", ErrStreamProtocol, f.ErrCode, f.ErrTitle)
	case FrameStop:
		return true, nil
	default:
		return false, fmt.Errorf("%w: unexpected event %q", ErrStreamProtocol, f.Event)
	}
	return false, nil
}

func serialBefore(a, b uint32) bool { return int32(a-b) < 0 }

// push inserts an inbound frame into the bounded reorder buffer in RTP order.
// A frame whose timestamp is already buffered is a duplicate.
func (s *MediaSession) push(f Frame) {
	i := sort.Search(len(s.jit), func(i int) bool { return !serialBefore(s.jit[i].Timestamp, f.Timestamp) })
	if i < len(s.jit) && s.jit[i].Timestamp == f.Timestamp {
		s.stats.Duplicates++
		return
	}
	s.jit = slices.Insert(s.jit, i, f)
}

func (s *MediaSession) pop() Frame {
	f := s.jit[0]
	s.jit = s.jit[1:]
	return f
}

func (s *MediaSession) flush() error {
	for len(s.jit) > 0 {
		if err := s.play(s.pop()); err != nil {
			return err
		}
	}
	return nil
}

// play places one in-order inbound frame on the sample timeline. Frames that
// do not advance the timeline are counted as stale; missing samples are
// reported as a gap so downstream recording can insert silence.
func (s *MediaSession) play(f Frame) error {
	pos, gap, err := s.clock.Position(f.Timestamp, s.next)
	if errors.Is(err, audio.ErrStaleFrame) {
		s.stats.Stale++
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStreamProtocol, err)
	}
	if len(f.Payload)%s.bps != 0 {
		return fmt.Errorf("%w: payload not whole samples", ErrStreamProtocol)
	}
	s.next = pos + int64(len(f.Payload)/s.bps)
	s.stats.GapSamples += gap
	select {
	case s.audio <- InboundAudio{Position: pos, Gap: gap, Payload: f.Payload}:
		s.stats.InboundChunks++
		return nil
	default:
		return ErrStreamBackpressure
	}
}

func (s *MediaSession) currentGen() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

func (s *MediaSession) sendChunk(ctx context.Context, conn *websocket.Conn, it outItem) error {
	select {
	case <-s.clear:
		if err := s.doClear(ctx, conn); err != nil {
			return err
		}
	default:
	}
	if it.gen != s.currentGen() {
		s.stats.OutboundDiscarded++
		return nil
	}
	name, err := s.ledger.Send(it.src, len(it.payload), s.cfg.Now())
	if err != nil {
		return err
	}
	media, err := MediaMessage(it.payload)
	if err != nil {
		return err
	}
	mark, err := MarkMessage(name)
	if err != nil {
		return err
	}
	if err := conn.Write(ctx, websocket.MessageText, media); err != nil {
		return err
	}
	if err := conn.Write(ctx, websocket.MessageText, mark); err != nil {
		return err
	}
	s.stats.OutboundChunks++
	return nil
}

func (s *MediaSession) doClear(ctx context.Context, conn *websocket.Conn) error {
	s.ledger.Clear(s.cfg.Now())
	return conn.Write(ctx, websocket.MessageText, ClearMessage())
}
