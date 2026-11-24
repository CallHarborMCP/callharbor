package audio

import (
	"errors"
	"fmt"
)

// RTPClock maps 32-bit wrapping RTP timestamps of one media stream onto a
// monotonic sample position relative to the first frame. Telnyx documents the
// media "timestamp" as RTP clock units, not wall-clock time.
type RTPClock struct {
	started bool
	base    uint32
	ext     int64
	last    uint32
}

// ErrStaleFrame reports a frame that does not advance the stream position
// (duplicate or reordered beyond the accepted window).
var ErrStaleFrame = errors.New("audio: stale or duplicate media frame")

// maxRTPJump bounds a forward jump that is still treated as loss (60 s at
// 8 kHz); larger jumps are protocol violations.
const maxRTPJump = 8000 * 60

// Position returns the sample offset of ts and the number of missing samples
// between the end of the previously accepted frame (prevEnd) and ts.
func (c *RTPClock) Position(ts uint32, prevEnd int64) (pos int64, gap int64, err error) {
	if !c.started {
		c.started = true
		c.base = ts
		c.last = ts
		return 0, 0, nil
	}
	delta := int64(int32(ts - c.last)) // #nosec G115 -- RFC 3550 serial-number wraparound is intended
	next := c.ext + delta
	if delta > maxRTPJump {
		return 0, 0, fmt.Errorf("audio: RTP timestamp jump of %d samples", delta)
	}
	if next < prevEnd {
		return 0, 0, fmt.Errorf("%w: position %d < expected %d", ErrStaleFrame, next, prevEnd)
	}
	c.ext = next
	c.last = ts
	return next, next - prevEnd, nil
}

// TwoPartyTimeline places inbound (remote party) and outbound (played agent)
// audio on one sample clock and produces interleaved stereo with silence for
// gaps. Channel 0 is the remote party, channel 1 the agent. Writes past the
// configured horizon are rejected to keep memory bounded.
type TwoPartyTimeline struct {
	rate    int
	horizon int64
	flushed int64
	ch      [2][]int16
	filled  [2]int64
	gaps    [2]int64
}

// NewTwoPartyTimeline creates a timeline at rate Hz holding at most horizon
// unflushed samples per channel.
func NewTwoPartyTimeline(rate int, horizon int64) (*TwoPartyTimeline, error) {
	if rate <= 0 || horizon <= 0 {
		return nil, fmt.Errorf("audio: invalid timeline rate=%d horizon=%d", rate, horizon)
	}
	return &TwoPartyTimeline{rate: rate, horizon: horizon}, nil
}

// ErrTimelineOverflow means a writer is too far ahead of the flush position.
var ErrTimelineOverflow = errors.New("audio: timeline horizon exceeded")

// Write places samples for channel at absolute sample position pos.
// Overlapping already-written or flushed regions are rejected.
func (t *TwoPartyTimeline) Write(channel int, pos int64, s []int16) error {
	if channel != 0 && channel != 1 {
		return fmt.Errorf("audio: invalid channel %d", channel)
	}
	if pos < t.filled[channel] {
		return fmt.Errorf("%w: channel %d pos %d < %d", ErrStaleFrame, channel, pos, t.filled[channel])
	}
	end := pos + int64(len(s))
	if end-t.flushed > t.horizon {
		return fmt.Errorf("%w: channel %d end %d flushed %d", ErrTimelineOverflow, channel, end, t.flushed)
	}
	gap := pos - t.filled[channel]
	t.gaps[channel] += gap
	buf := t.ch[channel]
	for i := int64(0); i < gap; i++ {
		buf = append(buf, 0)
	}
	t.ch[channel] = append(buf, s...)
	t.filled[channel] = end
	return nil
}

// Flush emits interleaved stereo up to upTo (absolute samples), filling any
// unwritten region of either channel with silence.
func (t *TwoPartyTimeline) Flush(dst []int16, upTo int64) []int16 {
	if upTo <= t.flushed {
		return dst
	}
	n := upTo - t.flushed
	for c := 0; c < 2; c++ {
		if t.filled[c] < upTo {
			missing := upTo - t.filled[c]
			t.gaps[c] += missing
			for i := int64(0); i < missing; i++ {
				t.ch[c] = append(t.ch[c], 0)
			}
			t.filled[c] = upTo
		}
	}
	for i := int64(0); i < n; i++ {
		dst = append(dst, t.ch[0][i], t.ch[1][i])
	}
	t.ch[0] = append(t.ch[0][:0], t.ch[0][n:]...)
	t.ch[1] = append(t.ch[1][:0], t.ch[1][n:]...)
	t.flushed = upTo
	return dst
}

// GapSamples returns silence inserted per channel (loss or idle).
func (t *TwoPartyTimeline) GapSamples() [2]int64 { return t.gaps }
