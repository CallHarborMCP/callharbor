package recording

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/audio"
)

// Sink receives sealed plaintext PCM. *Writer satisfies it.
type Sink interface {
	Write(p []byte) error
	Sync() error
	Close() error
}

// Quality flags. The vocabulary is fixed: it is persisted and exposed by the API.
const (
	FlagCallerGapFilled         = "caller_gap_filled"
	FlagCallerFramesDropped     = "caller_frames_dropped"
	FlagAgentPlayoutEstimated   = "agent_playout_estimated"
	FlagAgentInterrupted        = "agent_interrupted"
	FlagAgentPlayoutUnconfirmed = "agent_playout_unconfirmed"
	FlagWriteFailed             = "write_failed"
	FlagBackpressure            = "backpressure"
	FlagRecoveredAfterRestart   = "recovered_after_restart"
)

// Persisted recording statuses this package produces.
const (
	StatusComplete = "complete"
	StatusPartial  = "partial"
)

const (
	// reanchorLag is how far wall-clock may run ahead of the caller's media
	// position before the caller track is re-anchored forward.
	reanchorLag = 250 * time.Millisecond
	// horizonSeconds bounds audio held unflushed in the timeline. It must cover
	// the longest agent queue plus the flush lag, or a valid write overflows.
	horizonSeconds = maxPendingSeconds + 5
	// maxPendingSeconds caps agent audio queued for playback but not yet played.
	maxPendingSeconds = 120
	// writerQueue bounds plaintext chunks waiting for the writer goroutine.
	writerQueue = 256
	// syncInterval is the fsync cadence while writing.
	syncInterval = time.Second
)

// ErrRecorderClosed reports a Finish after the recorder already finished.
var ErrRecorderClosed = errors.New("recording: recorder finished")

// Config describes one recording. Leg is the stream's negotiated leg format,
// used to decode both tracks. Start is t0: the wall time of the stream start,
// which every sample position is measured from.
type Config struct {
	Leg   audio.Format
	Start time.Time
}

// Result is the terminal state of a recording.
type Result struct {
	Status       string
	ByteCount    int64 // PCM bytes durably handed to the sink, excluding any WAV header
	DurationMS   int64
	SampleRate   int
	QualityFlags []string
}

type pendingChunk struct {
	start   int64 // absolute sample position of the first sample
	samples []int16
}

// Recorder records one two-party call. Channel 0 is the remote party and
// channel 1 the agent audio that was played. Taps may be called from the media
// loop and from the cut path; they take one short lock and never wait on disk.
// The writer goroutine owns the sink.
type Recorder struct {
	mu sync.Mutex

	leg  audio.Format
	rate int
	t0   time.Time

	timeline *audio.TwoPartyTimeline
	flushed  int64

	// Caller track: anchor is the absolute sample of the RTP origin, relEnd the
	// RTP-relative end of the last accepted frame.
	clock   audio.RTPClock
	anchor  int64
	relEnd  int64
	started bool

	// Agent audio waiting for its play time, in playback order.
	pending        []pendingChunk
	pendingSamples int64

	out  chan []byte
	done chan struct{}

	writeFailed atomic.Bool
	written     atomic.Int64
	writeErr    error // set by the writer goroutine; read after done

	stopped       bool // writing stopped by backpressure
	gapFilled     bool
	framesDropped bool
	interrupted   bool
	backpressure  bool
	closed        bool
}

// New starts a recorder over sink and its writer goroutine. The sink is owned
// by the recorder from here: Finish closes it.
func New(cfg Config, sink Sink) (*Recorder, error) {
	if sink == nil {
		return nil, errors.New("recording: sink is required")
	}
	if err := cfg.Leg.Validate(); err != nil {
		return nil, fmt.Errorf("recording: leg format: %w", err)
	}
	rate := cfg.Leg.SampleRate
	if rate < 8000 || rate > 48000 {
		return nil, fmt.Errorf("%w: sample rate %d", ErrInvalidWAVFormat, rate)
	}
	if cfg.Start.IsZero() {
		return nil, errors.New("recording: start time is required")
	}
	tl, err := audio.NewTwoPartyTimeline(rate, int64(horizonSeconds*rate))
	if err != nil {
		return nil, fmt.Errorf("recording: timeline: %w", err)
	}
	r := &Recorder{
		leg:      cfg.Leg,
		rate:     rate,
		t0:       cfg.Start,
		timeline: tl,
		out:      make(chan []byte, writerQueue),
		done:     make(chan struct{}),
	}
	go r.run(sink)
	return r, nil
}

// sampleAt maps wall time to an absolute sample position from t0.
func (r *Recorder) sampleAt(t time.Time) int64 {
	d := t.Sub(r.t0)
	if d <= 0 {
		return 0
	}
	return r.samplesIn(d)
}

// samplesIn converts a duration to whole samples at the recording rate.
func (r *Recorder) samplesIn(d time.Duration) int64 {
	rate := int64(r.rate)
	return int64(d/time.Second)*rate + int64(d%time.Second)*rate/int64(time.Second)
}

// CallerPayload places one inbound frame. ts is the Telnyx RTP timestamp, payload
// the encoded leg audio, and at the wall time the frame arrived. Gaps are filled
// with silence; stale and duplicate frames are dropped and counted.
func (r *Recorder) CallerPayload(ts uint32, payload []byte, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	samples, err := audio.Decode(nil, r.leg, payload)
	if err != nil || len(samples) == 0 {
		r.framesDropped = true
		return
	}
	now := r.sampleAt(at)
	if !r.started {
		r.started = true
		r.anchor = now
	}
	pos, gap, err := r.clock.Position(ts, r.relEnd)
	if errors.Is(err, audio.ErrStaleFrame) {
		r.framesDropped = true
		return
	}
	if err != nil {
		// A timestamp jump beyond the accepted window restarts the track here.
		r.clock = audio.RTPClock{}
		r.anchor = now
		r.relEnd = 0
		r.gapFilled = true
		pos, gap, _ = r.clock.Position(ts, 0)
	}
	n := int64(len(samples))
	abs := r.anchor + pos
	if lag := now - (abs + n); lag > r.samplesIn(reanchorLag) {
		// Wall-clock is ahead of the media position: move the anchor forward
		// only. Silence fills the skipped span, and the track never goes back.
		r.anchor += lag
		abs += lag
		r.gapFilled = true
	}
	if gap > 0 {
		r.gapFilled = true
	}
	if err := r.timeline.Write(0, abs, samples); err != nil {
		// Behind the flush point or overlapping written audio.
		r.framesDropped = true
		return
	}
	r.relEnd = pos + n
	r.advance(at)
}

// AgentPayload queues agent audio that will play starting at playStart. It is
// written to the timeline only once its play time has passed, so audio gated or
// cut before it played never reaches the recording. at is the current wall time.
func (r *Recorder) AgentPayload(playStart time.Time, payload []byte, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	samples, err := audio.Decode(nil, r.leg, payload)
	if err != nil || len(samples) == 0 {
		return
	}
	n := int64(len(samples))
	if r.pendingSamples+n > int64(maxPendingSeconds*r.rate) {
		r.backpressure = true
		return
	}
	r.pending = append(r.pending, pendingChunk{start: r.sampleAt(playStart), samples: samples})
	r.pendingSamples += n
	r.advance(at)
}

// Cut records an interruption at wall time at. Agent audio that had already
// played is kept; audio queued after the cut is dropped, and the one chunk the
// cut landed in is truncated at the cut point.
func (r *Recorder) Cut(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.interrupted = true
	r.truncateLocked(r.sampleAt(at))
	r.advance(at)
}

// truncateLocked drops pending audio at or after limit and cuts the chunk that
// straddles it. The caller holds mu.
func (r *Recorder) truncateLocked(limit int64) {
	var kept []pendingChunk
	for _, p := range r.pending {
		n := int64(len(p.samples))
		if p.start >= limit {
			r.pendingSamples -= n
			continue
		}
		if end := p.start + n; end > limit {
			keep := limit - p.start
			r.pendingSamples -= n - keep
			p.samples = p.samples[:keep]
		}
		kept = append(kept, p)
	}
	r.pending = kept
}

// commitLocked writes pending agent chunks whose play time has ended by limit.
// The queue is in playback order, so it stops at the first chunk not yet due.
func (r *Recorder) commitLocked(limit int64) {
	i := 0
	for ; i < len(r.pending); i++ {
		p := r.pending[i]
		end := p.start + int64(len(p.samples))
		if end > limit {
			break
		}
		r.pendingSamples -= int64(len(p.samples))
		// A rejected write is an overlap or a late chunk. The agent audio is
		// simply absent from the recording; the flags describe the playout.
		_ = r.timeline.Write(1, p.start, p.samples)
	}
	r.pending = r.pending[i:]
}

// advance commits due agent audio and flushes the timeline up to one second
// behind wall-clock to the writer. The caller holds mu.
func (r *Recorder) advance(at time.Time) {
	now := r.sampleAt(at)
	r.commitLocked(now)
	upTo := now - int64(r.rate)
	// Never flush past the earliest agent chunk still waiting to play. Flushing
	// pads its channel with silence, so the chunk would then be rejected as stale.
	if len(r.pending) > 0 && r.pending[0].start < upTo {
		upTo = r.pending[0].start
	}
	if upTo <= r.flushed {
		return
	}
	dst := r.timeline.Flush(nil, upTo)
	r.flushed = upTo
	r.emitLocked(dst)
}

// emitLocked hands interleaved samples to the writer without blocking. A full
// queue stops writing for the rest of the call; the call itself keeps running.
func (r *Recorder) emitLocked(s []int16) {
	if len(s) == 0 || r.stopped || r.writeFailed.Load() {
		return
	}
	buf := make([]byte, 2*len(s))
	for i, v := range s {
		binary.LittleEndian.PutUint16(buf[2*i:], uint16(v)) // #nosec G115 -- two's-complement sample bytes
	}
	select {
	case r.out <- buf:
	default:
		r.stopped = true
		r.backpressure = true
	}
}

// run is the writer goroutine. It owns the sink until out is closed.
func (r *Recorder) run(sink Sink) {
	defer close(r.done)
	lastSync := time.Now()
	for buf := range r.out {
		if r.writeFailed.Load() {
			continue
		}
		if err := sink.Write(buf); err != nil {
			r.fail(err)
			continue
		}
		r.written.Add(int64(len(buf)))
		if time.Since(lastSync) >= syncInterval {
			if err := sink.Sync(); err != nil {
				r.fail(err)
				continue
			}
			lastSync = time.Now()
		}
	}
	if err := sink.Close(); err != nil && !r.writeFailed.Load() {
		r.fail(err)
	}
}

func (r *Recorder) fail(err error) {
	r.writeErr = err
	r.writeFailed.Store(true)
}

// Abort stops a recorder that never took media: the writer closes and nothing
// is flushed. It is for setup failures after New, before any frame.
func (r *Recorder) Abort() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrRecorderClosed
	}
	r.closed = true
	close(r.out)
	r.mu.Unlock()
	<-r.done
	return nil
}

// Finish ends the recording at wall time at. Pending agent audio at or after the
// end is dropped, the rest is truncated to it, the timeline is flushed, and the
// file is sealed and closed. The result is complete unless the recording
// degraded (backpressure or a write failure), in which case it is partial.
// unconfirmed reports agent audio whose playout mark never echoed.
func (r *Recorder) Finish(at time.Time, unconfirmed bool) (Result, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return Result{}, ErrRecorderClosed
	}
	r.closed = true
	end := r.sampleAt(at)
	r.truncateLocked(end)
	r.commitLocked(end)
	if end > r.flushed {
		dst := r.timeline.Flush(nil, end)
		r.flushed = end
		r.emitLocked(dst)
	}
	close(r.out)
	r.mu.Unlock()

	<-r.done
	res := Result{
		Status:       StatusComplete,
		ByteCount:    r.written.Load(),
		SampleRate:   r.rate,
		QualityFlags: r.flags(unconfirmed),
	}
	if r.backpressure || r.writeFailed.Load() {
		res.Status = StatusPartial
	}
	res.DurationMS = res.ByteCount / 4 * 1000 / int64(r.rate)
	return res, r.writeErr
}

// flags returns the sorted quality flags for the finished recording.
func (r *Recorder) flags(unconfirmed bool) []string {
	f := []string{FlagAgentPlayoutEstimated}
	add := func(cond bool, flag string) {
		if cond {
			f = append(f, flag)
		}
	}
	add(r.gapFilled, FlagCallerGapFilled)
	add(r.framesDropped, FlagCallerFramesDropped)
	add(r.interrupted, FlagAgentInterrupted)
	add(unconfirmed, FlagAgentPlayoutUnconfirmed)
	add(r.writeFailed.Load(), FlagWriteFailed)
	add(r.backpressure, FlagBackpressure)
	sort.Strings(f)
	return f
}
